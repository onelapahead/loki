package bloomgateway

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"sync"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/grpcclient"
	"github.com/grafana/dskit/instrument"
	"github.com/grafana/dskit/ring"
	ringclient "github.com/grafana/dskit/ring/client"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"golang.org/x/exp/slices"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/grafana/loki/pkg/bloomutils"
	"github.com/grafana/loki/pkg/distributor/clientpool"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql/syntax"
	"github.com/grafana/loki/pkg/logqlmodel/stats"
	"github.com/grafana/loki/pkg/queue"
	v1 "github.com/grafana/loki/pkg/storage/bloom/v1"
	"github.com/grafana/loki/pkg/storage/chunk/cache"
	"github.com/grafana/loki/pkg/storage/chunk/cache/resultscache"
	"github.com/grafana/loki/pkg/util/constants"
)

var (
	// BlocksOwnerRead is the operation used to check the authoritative owners of a block
	// (replicas included) that are available for queries (a bloom gateway is available for
	// queries only when ACTIVE).
	BlocksOwnerRead = ring.NewOp([]ring.InstanceState{ring.ACTIVE}, nil)
	// groupedChunksRefPool pooling slice of logproto.GroupedChunkRefs [64, 128, 256, ..., 65536]
	groupedChunksRefPool = queue.NewSlicePool[*logproto.GroupedChunkRefs](1<<6, 1<<16, 2)
	// ringGetBuffersPool pooling for ringGetBuffers to avoid calling ring.MakeBuffersForGet() for each request
	ringGetBuffersPool = sync.Pool{
		New: func() interface{} {
			descs, hosts, zones := ring.MakeBuffersForGet()
			return &ringGetBuffers{
				Descs: descs,
				Hosts: hosts,
				Zones: zones,
			}
		},
	}
)

type ringGetBuffers struct {
	Descs []ring.InstanceDesc
	Hosts []string
	Zones []string
}

func (buf *ringGetBuffers) Reset() {
	buf.Descs = buf.Descs[:0]
	buf.Hosts = buf.Hosts[:0]
	buf.Zones = buf.Zones[:0]
}

// GRPCPool represents a pool of gRPC connections to different bloom gateway instances.
// Interfaces are inlined for simplicity to automatically satisfy interface functions.
type GRPCPool struct {
	grpc_health_v1.HealthClient
	logproto.BloomGatewayClient
	io.Closer
}

// NewBloomGatewayGRPCPool instantiates a new pool of GRPC connections for the Bloom Gateway
// Internally, it also instantiates a protobuf bloom gateway client and a health client.
func NewBloomGatewayGRPCPool(address string, opts []grpc.DialOption) (*GRPCPool, error) {
	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "new grpc pool dial")
	}

	return &GRPCPool{
		Closer:             conn,
		HealthClient:       grpc_health_v1.NewHealthClient(conn),
		BloomGatewayClient: logproto.NewBloomGatewayClient(conn),
	}, nil
}

// IndexGatewayClientConfig configures the Index Gateway client used to
// communicate with the Index Gateway server.
type ClientConfig struct {
	// PoolConfig defines the behavior of the gRPC connection pool used to communicate
	// with the Bloom Gateway.
	// It is defined at the distributors YAML section and reused here.
	PoolConfig clientpool.PoolConfig `yaml:"pool_config,omitempty" doc:"description=Configures the behavior of the connection pool."`

	// GRPCClientConfig configures the gRPC connection between the Bloom Gateway client and the server.
	GRPCClientConfig grpcclient.Config `yaml:"grpc_client_config"`

	// LogGatewayRequests configures if requests sent to the gateway should be logged or not.
	// The log messages are of type debug and contain the address of the gateway and the relevant tenant.
	LogGatewayRequests bool `yaml:"log_gateway_requests"`

	// Ring is the Bloom Gateway ring used to find the appropriate Bloom Gateway instance
	// this client should talk to.
	Ring ring.ReadRing `yaml:"-"`

	// Cache configures the cache used to store the results of the Bloom Gateway server.
	Cache        CacheConfig `yaml:"results_cache,omitempty"`
	CacheResults bool        `yaml:"cache_results"`
}

// RegisterFlags registers flags for the Bloom Gateway client configuration.
func (i *ClientConfig) RegisterFlags(f *flag.FlagSet) {
	i.RegisterFlagsWithPrefix("bloom-gateway-client.", f)
}

// RegisterFlagsWithPrefix registers flags for the Bloom Gateway client configuration with a common prefix.
func (i *ClientConfig) RegisterFlagsWithPrefix(prefix string, f *flag.FlagSet) {
	i.GRPCClientConfig.RegisterFlagsWithPrefix(prefix+"grpc", f)
	i.Cache.RegisterFlagsWithPrefix(prefix+"cache.", f)
	f.BoolVar(&i.CacheResults, prefix+"cache_results", false, "Flag to control whether to cache bloom gateway client requests/responses.")
	f.BoolVar(&i.LogGatewayRequests, prefix+"log-gateway-requests", false, "Flag to control whether requests sent to the gateway should be logged or not.")
}

func (i *ClientConfig) Validate() error {
	if err := i.GRPCClientConfig.Validate(); err != nil {
		return errors.Wrap(err, "grpc client config")
	}

	if i.CacheResults {
		if err := i.Cache.Validate(); err != nil {
			return errors.Wrap(err, "cache config")
		}
	}

	return nil
}

type Client interface {
	FilterChunks(ctx context.Context, tenant string, from, through model.Time, groups []*logproto.GroupedChunkRefs, filters ...syntax.LineFilter) ([]*logproto.GroupedChunkRefs, error)
}

type GatewayClient struct {
	cfg    ClientConfig
	limits Limits
	logger log.Logger
	pool   *ringclient.Pool
	ring   ring.ReadRing
}

func NewClient(
	cfg ClientConfig,
	readRing ring.ReadRing,
	limits Limits,
	registerer prometheus.Registerer,
	logger log.Logger,
	metricsNamespace string,
	cacheGen resultscache.CacheGenNumberLoader,
	retentionEnabled bool,
) (*GatewayClient, error) {
	latency := promauto.With(registerer).NewHistogramVec(prometheus.HistogramOpts{
		Namespace: constants.Loki,
		Subsystem: "bloom_gateway",
		Name:      "request_duration_seconds",
		Help:      "Time (in seconds) spent serving requests when using the bloom gateway",
		Buckets:   instrument.DefBuckets,
	}, []string{"operation", "status_code"})

	dialOpts, err := cfg.GRPCClientConfig.DialOption(grpcclient.Instrument(latency))
	if err != nil {
		return nil, err
	}

	var c cache.Cache
	if cfg.CacheResults {
		c, err = cache.New(cfg.Cache.CacheConfig, registerer, logger, stats.BloomFilterCache, constants.Loki)
		if err != nil {
			return nil, errors.Wrap(err, "new bloom gateway cache")
		}
		if cfg.Cache.Compression == "snappy" {
			c = cache.NewSnappy(c, logger)
		}
	}

	poolFactory := func(addr string) (ringclient.PoolClient, error) {
		pool, err := NewBloomGatewayGRPCPool(addr, dialOpts)
		if err != nil {
			return nil, errors.Wrap(err, "new bloom gateway grpc pool")
		}

		if cfg.CacheResults {
			pool.BloomGatewayClient = NewBloomGatewayClientCacheMiddleware(
				logger,
				pool.BloomGatewayClient,
				c,
				limits,
				cacheGen,
				retentionEnabled,
			)
		}

		return pool, nil
	}

	return &GatewayClient{
		cfg:    cfg,
		logger: logger,
		limits: limits,
		pool:   clientpool.NewPool("bloom-gateway", cfg.PoolConfig, cfg.Ring, ringclient.PoolAddrFunc(poolFactory), logger, metricsNamespace),
		ring:   readRing,
	}, nil
}

func shuffleAddrs(addrs []string) []string {
	rand.Shuffle(len(addrs), func(i, j int) {
		addrs[i], addrs[j] = addrs[j], addrs[i]
	})
	return addrs
}

// FilterChunkRefs implements Client
func (c *GatewayClient) FilterChunks(ctx context.Context, tenant string, from, through model.Time, groups []*logproto.GroupedChunkRefs, filters ...syntax.LineFilter) ([]*logproto.GroupedChunkRefs, error) {
	if !c.limits.BloomGatewayEnabled(tenant) {
		return groups, nil
	}

	subRing := GetShuffleShardingSubring(c.ring, tenant, c.limits)
	rs, err := subRing.GetAllHealthy(BlocksOwnerRead)
	if err != nil {
		return nil, errors.Wrap(err, "bloom gateway get healthy instances")
	}

	servers, err := serverAddressesWithTokenRanges(subRing, rs.Instances)
	if err != nil {
		return nil, err
	}
	streamsByInst := groupFingerprintsByServer(groups, servers)

	filteredChunkRefs := groupedChunksRefPool.Get(len(groups))
	defer groupedChunksRefPool.Put(filteredChunkRefs)

	for _, item := range streamsByInst {
		// randomize order of addresses so we don't hotspot the first server in the list
		addrs := shuffleAddrs(item.instance.addrs)
		err := c.doForAddrs(addrs, func(client logproto.BloomGatewayClient) error {
			req := &logproto.FilterChunkRefRequest{
				From:    from,
				Through: through,
				Refs:    item.fingerprints,
				Filters: filters,
			}
			resp, err := client.FilterChunkRefs(ctx, req)
			if err != nil {
				return err
			}
			filteredChunkRefs = append(filteredChunkRefs, resp.ChunkRefs...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return filteredChunkRefs, nil
}

// doForAddrs sequetially calls the provided callback function fn for each
// address in given slice addrs until the callback function does not return an
// error.
func (c *GatewayClient) doForAddrs(addrs []string, fn func(logproto.BloomGatewayClient) error) error {
	var err error
	var poolClient ringclient.PoolClient

	for _, addr := range addrs {
		poolClient, err = c.pool.GetClientFor(addr)
		if err != nil {
			level.Error(c.logger).Log("msg", fmt.Sprintf("failed to get client for instance %s", addr), "err", err)
			continue
		}
		err = fn(poolClient.(logproto.BloomGatewayClient))
		if err != nil {
			level.Error(c.logger).Log("msg", fmt.Sprintf("client do failed for instance %s", addr), "err", err)
			continue
		}
		return nil
	}
	return err
}

func groupFingerprintsByServer(groups []*logproto.GroupedChunkRefs, servers []addrsWithBounds) []instanceWithFingerprints {
	boundedFingerprints := partitionFingerprintsByAddresses(groups, servers)
	return groupByInstance(boundedFingerprints)
}

func mapTokenRangeToFingerprintRange(r bloomutils.Range[uint32]) v1.FingerprintBounds {
	minFp := uint64(r.Min) << 32
	maxFp := uint64(r.Max) << 32
	return v1.NewBounds(
		model.Fingerprint(minFp),
		model.Fingerprint(maxFp|math.MaxUint32),
	)
}

func serverAddressesWithTokenRanges(subRing ring.ReadRing, instances []ring.InstanceDesc) ([]addrsWithBounds, error) {
	bufDescs, bufHosts, bufZones := ring.MakeBuffersForGet()

	servers := make([]addrsWithBounds, 0, len(instances))
	it := bloomutils.NewInstanceSortMergeIterator(instances)

	for it.Next() {
		// We can use on of the tokens from the token range
		// to obtain all addresses for that token.
		rs, err := subRing.Get(it.At().TokenRange.Max, BlocksOwnerRead, bufDescs, bufHosts, bufZones)
		if err != nil {
			return nil, errors.Wrap(err, "bloom gateway get ring")
		}

		bounds := mapTokenRangeToFingerprintRange(it.At().TokenRange)
		servers = append(servers, addrsWithBounds{
			id:                it.At().Instance.Id,
			addrs:             rs.GetAddresses(),
			FingerprintBounds: bounds,
		})
	}

	if len(servers) > 0 && servers[len(servers)-1].Max < math.MaxUint64 {
		// append the instance for the range between the maxFp and MaxUint64
		// TODO(owen-d): support wrapping around keyspace for token ranges
		servers = append(servers, addrsWithBounds{
			id:    servers[0].id,
			addrs: servers[0].addrs,
			FingerprintBounds: v1.NewBounds(
				servers[len(servers)-1].Max+1,
				model.Fingerprint(math.MaxUint64),
			),
		})
	}
	return servers, nil
}

type addrsWithBounds struct {
	v1.FingerprintBounds
	id    string
	addrs []string
}

type instanceWithFingerprints struct {
	instance     addrsWithBounds
	fingerprints []*logproto.GroupedChunkRefs
}

func partitionFingerprintsByAddresses(fingerprints []*logproto.GroupedChunkRefs, addresses []addrsWithBounds) (result []instanceWithFingerprints) {
	for _, instance := range addresses {
		min, _ := slices.BinarySearchFunc(fingerprints, instance.FingerprintBounds, func(g *logproto.GroupedChunkRefs, b v1.FingerprintBounds) int {
			if g.Fingerprint < uint64(b.Min) {
				return -1
			} else if g.Fingerprint > uint64(b.Min) {
				return 1
			}
			return 0
		})

		max, _ := slices.BinarySearchFunc(fingerprints, instance.FingerprintBounds, func(g *logproto.GroupedChunkRefs, b v1.FingerprintBounds) int {
			if g.Fingerprint <= uint64(b.Max) {
				return -1
			} else if g.Fingerprint > uint64(b.Max) {
				return 1
			}
			return 0
		})

		// fingerprint is out of boundaries
		if min == len(fingerprints) || max == 0 {
			continue
		}

		result = append(result, instanceWithFingerprints{instance: instance, fingerprints: fingerprints[min:max]})
	}

	return result
}

// groupByInstance groups fingerprints by server instance
func groupByInstance(boundedFingerprints []instanceWithFingerprints) []instanceWithFingerprints {
	if len(boundedFingerprints) == 0 {
		return []instanceWithFingerprints{}
	}

	result := make([]instanceWithFingerprints, 0, len(boundedFingerprints))
	pos := make(map[string]int, len(boundedFingerprints))

	for _, cur := range boundedFingerprints {
		if len(cur.fingerprints) == 0 {
			continue
		}
		// Copy fingerprint slice, otherwise we mutate the original
		// TODO(chaudum): Use SlicePool
		tmp := make([]*logproto.GroupedChunkRefs, len(cur.fingerprints))
		_ = copy(tmp, cur.fingerprints)

		idx, ok := pos[cur.instance.id]
		if ok {
			result[idx].fingerprints = append(result[idx].fingerprints, tmp...)
			continue
		}

		pos[cur.instance.id] = len(result)
		result = append(result, instanceWithFingerprints{
			instance: addrsWithBounds{
				id:    cur.instance.id,
				addrs: cur.instance.addrs,
			},
			fingerprints: tmp,
		})
	}

	return result
}

// GetShuffleShardingSubring returns the subring to be used for a given user.
// This function should be used both by index gateway servers and clients in
// order to guarantee the same logic is used.
func GetShuffleShardingSubring(ring ring.ReadRing, tenantID string, limits Limits) ring.ReadRing {
	shardSize := limits.BloomGatewayShardSize(tenantID)

	// A shard size of 0 means shuffle sharding is disabled for this specific user,
	// so we just return the full ring so that indexes will be sharded across all index gateways.
	// Since we set the shard size to replication factor if shard size is 0, this
	// can only happen if both the shard size and the replication factor are set
	// to 0.
	if shardSize <= 0 {
		return ring
	}

	return ring.ShuffleShard(tenantID, shardSize)
}
