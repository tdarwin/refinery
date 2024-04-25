package route

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	jsoniter "github.com/json-iterator/go"
	"github.com/klauspost/compress/zstd"
	"github.com/pelletier/go-toml/v2"
	"github.com/vmihailenco/msgpack/v5"
	"google.golang.org/grpc"
	healthserver "google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"gopkg.in/yaml.v3"

	// grpc/gzip compressor, auto registers on import
	_ "google.golang.org/grpc/encoding/gzip"

	huskyotlp "github.com/honeycombio/husky/otlp"

	"github.com/honeycombio/refinery/collect"
	"github.com/honeycombio/refinery/config"
	"github.com/honeycombio/refinery/internal/health"
	"github.com/honeycombio/refinery/logger"
	"github.com/honeycombio/refinery/metrics"
	"github.com/honeycombio/refinery/transmit"
	"github.com/honeycombio/refinery/types"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

const (
	// numZstdDecoders is set statically here - we may make it into a config option
	// A normal practice might be to use some multiple of the CPUs, but that goes south
	// in kubernetes
	numZstdDecoders        = 4
	traceIDShortLength     = 8
	traceIDLongLength      = 16
	GRPCMessageSizeMax int = 5000000 // 5MB
	defaultSampleRate      = 1
)

type Router struct {
	Config               config.Config         `inject:""`
	Logger               logger.Logger         `inject:""`
	Health               health.Reporter       `inject:""`
	HTTPTransport        *http.Transport       `inject:"upstreamTransport"`
	UpstreamTransmission transmit.Transmission `inject:"upstreamTransmission"`
	Collector            collect.Collector     `inject:"collector"`
	Metrics              metrics.Metrics       `inject:"genericMetrics"`

	// version is set on startup so that the router may answer HTTP requests for
	// the version
	versionStr string

	proxyClient *http.Client

	// iopLogger is a logger that knows whether it's incoming or peer
	iopLogger iopLogger

	zstdDecoders chan *zstd.Decoder

	server     *http.Server
	grpcServer *grpc.Server
	doneWG     sync.WaitGroup
	donech     chan struct{}

	environmentCache *environmentCache
	hsrv             *healthserver.Server
}

type BatchResponse struct {
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
}

type iopLogger struct {
	logger.Logger
	incomingOrPeer string
}

func (i *iopLogger) Debug() logger.Entry {
	return i.Logger.Debug().WithField("router_iop", i.incomingOrPeer)
}

func (i *iopLogger) Info() logger.Entry {
	return i.Logger.Info().WithField("router_iop", i.incomingOrPeer)
}

func (i *iopLogger) Error() logger.Entry {
	return i.Logger.Error().WithField("router_iop", i.incomingOrPeer)
}

func (r *Router) SetVersion(ver string) {
	r.versionStr = ver
}

// LnS spins up the Listen and Serve portion of the router. A router is
// initialized as being for either incoming traffic from clients or traffic from
// a peer. They listen on different addresses so peer traffic can be
// prioritized.
func (r *Router) LnS() {
	r.iopLogger = iopLogger{
		Logger: r.Logger,
	}

	r.proxyClient = &http.Client{
		Timeout:   time.Second * 10,
		Transport: r.HTTPTransport,
	}
	r.environmentCache = newEnvironmentCache(r.Config.GetEnvironmentCacheTTL(), r.lookupEnvironment)

	var err error
	r.zstdDecoders, err = makeDecoders(numZstdDecoders)
	if err != nil {
		r.iopLogger.Error().Logf("couldn't start zstd decoders: %s", err.Error())
		return
	}

	r.Metrics.Register("incoming_router_proxied", "counter")
	r.Metrics.Register("incoming_router_event", "counter")
	r.Metrics.Register("incoming_router_batch", "counter")
	r.Metrics.Register("incoming_router_nonspan", "counter")
	r.Metrics.Register("incoming_router_span", "counter")
	r.Metrics.Register("incoming_router_peer", "counter")
	r.Metrics.Register("incoming_router_dropped", "counter")

	muxxer := mux.NewRouter()

	muxxer.Use(r.setResponseHeaders)
	muxxer.Use(r.requestLogger)
	muxxer.Use(r.panicCatcher)

	// answer a basic health check locally
	muxxer.HandleFunc("/alive", r.alive).Name("local health")
	muxxer.HandleFunc("/ready", r.ready).Name("local readiness")
	muxxer.HandleFunc("/panic", r.panic).Name("intentional panic")
	muxxer.HandleFunc("/version", r.version).Name("report version info")

	// require a local auth for query usage
	queryMuxxer := muxxer.PathPrefix("/query/").Methods("GET").Subrouter()
	queryMuxxer.Use(r.queryTokenChecker)

	queryMuxxer.HandleFunc("/trace/{traceID}", r.debugTrace).Name("get debug information for given trace ID")
	queryMuxxer.HandleFunc("/rules/{format}/{dataset}", r.getSamplerRules).Name("get formatted sampler rules for given dataset")
	queryMuxxer.HandleFunc("/allrules/{format}", r.getAllSamplerRules).Name("get formatted sampler rules for all datasets")
	queryMuxxer.HandleFunc("/configmetadata", r.getConfigMetadata).Name("get configuration metadata")

	// require an auth header for events and batches
	authedMuxxer := muxxer.PathPrefix("/1/").Methods("POST").Subrouter()
	authedMuxxer.UseEncodedPath()
	authedMuxxer.Use(r.apiKeyChecker)

	// handle events and batches
	authedMuxxer.HandleFunc("/events/{datasetName}", r.event).Name("event")
	authedMuxxer.HandleFunc("/batch/{datasetName}", r.batch).Name("batch")

	// require an auth header for OTLP requests
	r.AddOTLPMuxxer(muxxer)

	// pass everything else through unmolested
	muxxer.PathPrefix("/").HandlerFunc(r.proxy).Name("proxy")

	listenAddr := r.Config.GetListenAddr()
	if err != nil {
		r.iopLogger.Error().Logf("failed to get listen addr config: %s", err)
		return
	}
	// GRPC listen addr is optional, err means addr was not empty and invalid
	grpcAddr := r.Config.GetGRPCListenAddr()
	if err != nil {
		r.iopLogger.Error().Logf("failed to get grpc listen addr config: %s", err)
		return
	}

	r.iopLogger.Info().Logf("Listening on %s", listenAddr)
	r.server = &http.Server{
		Addr:        listenAddr,
		Handler:     muxxer,
		IdleTimeout: r.Config.GetHTTPIdleTimeout(),
	}

	r.donech = make(chan struct{})
	if r.Config.GetGRPCEnabled() && len(grpcAddr) > 0 {
		l, err := net.Listen("tcp", grpcAddr)
		if err != nil {
			r.iopLogger.Error().Logf("failed to listen to grpc addr: " + grpcAddr)
		}

		r.iopLogger.Info().Logf("gRPC listening on %s", grpcAddr)
		grpcConfig := r.Config.GetGRPCConfig()
		serverOpts := []grpc.ServerOption{
			grpc.MaxSendMsgSize(int(grpcConfig.MaxSendMsgSize)),
			grpc.MaxRecvMsgSize(int(grpcConfig.MaxRecvMsgSize)),
			grpc.KeepaliveParams(keepalive.ServerParameters{
				MaxConnectionIdle:     time.Duration(grpcConfig.MaxConnectionIdle),
				MaxConnectionAge:      time.Duration(grpcConfig.MaxConnectionAge),
				MaxConnectionAgeGrace: time.Duration(grpcConfig.MaxConnectionAgeGrace),
				Time:                  time.Duration(grpcConfig.KeepAlive),
				Timeout:               time.Duration(grpcConfig.KeepAliveTimeout),
			}),
		}
		r.grpcServer = grpc.NewServer(serverOpts...)

		traceServer := NewTraceServer(r)
		collectortrace.RegisterTraceServiceServer(r.grpcServer, traceServer)

		logsServer := NewLogsServer(r)
		collectorlogs.RegisterLogsServiceServer(r.grpcServer, logsServer)

		grpc_health_v1.RegisterHealthServer(r.grpcServer, r)
		go r.grpcServer.Serve(l)
	}

	r.doneWG.Add(1)
	go func() {
		defer r.doneWG.Done()

		err = r.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.iopLogger.Error().Logf("failed to ListenAndServe: %s", err)
		}
	}()
}

func (r *Router) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	err := r.server.Shutdown(ctx)
	if err != nil {
		return err
	}
	if r.grpcServer != nil {
		r.grpcServer.GracefulStop()
	}
	close(r.donech)
	r.doneWG.Wait()
	return nil
}

func (r *Router) alive(w http.ResponseWriter, req *http.Request) {
	r.iopLogger.Debug().Logf("answered /alive check")
	alive := r.Health.IsAlive()
	if !alive {
		w.WriteHeader(http.StatusServiceUnavailable)
		r.marshalToFormat(w, map[string]interface{}{"source": "refinery", "alive": "no"}, "json")
		return
	}
	r.marshalToFormat(w, map[string]interface{}{"source": "refinery", "alive": "yes"}, "json")
}

func (r *Router) ready(w http.ResponseWriter, req *http.Request) {
	r.iopLogger.Debug().Logf("answered /ready check")
	ready := r.Health.IsReady()
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		r.marshalToFormat(w, map[string]interface{}{"source": "refinery", "ready": "no"}, "json")
		return
	}
	r.marshalToFormat(w, map[string]interface{}{"source": "refinery", "ready": "yes"}, "json")
}

func (r *Router) panic(w http.ResponseWriter, req *http.Request) {
	panic("panic? never!")
}

func (r *Router) version(w http.ResponseWriter, req *http.Request) {
	w.Write([]byte(fmt.Sprintf(`{"source":"refinery","version":"%s"}`, r.versionStr)))
}

func (r *Router) debugTrace(w http.ResponseWriter, req *http.Request) {
	traceID := mux.Vars(req)["traceID"]
	w.Write([]byte(fmt.Sprintf(`{"traceID":"%s"}`, traceID)))
}

func (r *Router) getSamplerRules(w http.ResponseWriter, req *http.Request) {
	format := strings.ToLower(mux.Vars(req)["format"])
	dataset := mux.Vars(req)["dataset"]
	cfg, name, err := r.Config.GetSamplerConfigForDestName(dataset)
	if err != nil {
		w.Write([]byte(fmt.Sprintf("got error %v trying to fetch config for dataset %s\n", err, dataset)))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.marshalToFormat(w, map[string]interface{}{name: cfg}, format)
}

func (r *Router) getAllSamplerRules(w http.ResponseWriter, req *http.Request) {
	format := strings.ToLower(mux.Vars(req)["format"])
	cfgs := r.Config.GetAllSamplerRules()
	r.marshalToFormat(w, cfgs, format)
}

func (r *Router) getConfigMetadata(w http.ResponseWriter, req *http.Request) {
	cm := r.Config.GetConfigMetadata()
	r.marshalToFormat(w, cm, "json")
}

func (r *Router) marshalToFormat(w http.ResponseWriter, obj interface{}, format string) {
	var body []byte
	var err error
	switch format {
	case "json":
		body, err = json.Marshal(obj)
		if err != nil {
			w.Write([]byte(fmt.Sprintf("got error %v trying to marshal to json\n", err)))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	case "toml":
		body, err = toml.Marshal(obj)
		if err != nil {
			w.Write([]byte(fmt.Sprintf("got error %v trying to marshal to toml\n", err)))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	case "yaml":
		body, err = yaml.Marshal(obj)
		if err != nil {
			w.Write([]byte(fmt.Sprintf("got error %v trying to marshal to toml\n", err)))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	default:
		w.Write([]byte(fmt.Sprintf("invalid format '%s' when marshaling\n", format)))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/"+format)
	w.Write(body)
}

// event is handler for /1/event/
func (r *Router) event(w http.ResponseWriter, req *http.Request) {
	r.Metrics.Increment("incoming_router_event")
	defer req.Body.Close()

	bodyReader, err := r.getMaybeCompressedBody(req)
	if err != nil {
		r.handlerReturnWithError(w, ErrPostBody, err)
		return
	}

	reqBod, err := io.ReadAll(bodyReader)
	if err != nil {
		r.handlerReturnWithError(w, ErrPostBody, err)
		return
	}

	ev, err := r.requestToEvent(req, reqBod)
	if err != nil {
		r.handlerReturnWithError(w, ErrReqToEvent, err)
		return
	}

	reqID := req.Context().Value(types.RequestIDContextKey{})
	err = r.processEvent(ev, reqID)
	if err != nil {
		r.handlerReturnWithError(w, ErrReqToEvent, err)
		return
	}
}

func (r *Router) requestToEvent(req *http.Request, reqBod []byte) (*types.Event, error) {
	// get necessary bits out of the incoming event
	apiKey := req.Header.Get(types.APIKeyHeader)
	if apiKey == "" {
		apiKey = req.Header.Get(types.APIKeyHeaderShort)
	}
	sampleRate, err := strconv.Atoi(req.Header.Get(types.SampleRateHeader))
	if err != nil {
		sampleRate = 1
	}
	eventTime := getEventTime(req.Header.Get(types.TimestampHeader))
	dataset, err := getDatasetFromRequest(req)
	if err != nil {
		return nil, err
	}

	apiHost, err := r.Config.GetHoneycombAPI()
	if err != nil {
		return nil, err
	}

	// get environment name - will be empty for legacy keys
	environment, err := r.getEnvironmentName(apiKey)
	if err != nil {
		return nil, err
	}

	data := map[string]interface{}{}
	err = unmarshal(req, bytes.NewReader(reqBod), &data)
	if err != nil {
		return nil, err
	}

	return &types.Event{
		Context:     req.Context(),
		APIHost:     apiHost,
		APIKey:      apiKey,
		Dataset:     dataset,
		Environment: environment,
		SampleRate:  uint(sampleRate),
		Timestamp:   eventTime,
		Data:        data,
	}, nil
}

func (r *Router) batch(w http.ResponseWriter, req *http.Request) {
	r.Metrics.Increment("incoming_router_batch")
	defer req.Body.Close()

	reqID := req.Context().Value(types.RequestIDContextKey{})
	debugLog := r.iopLogger.Debug().WithField("request_id", reqID)

	bodyReader, err := r.getMaybeCompressedBody(req)
	if err != nil {
		r.handlerReturnWithError(w, ErrPostBody, err)
		return
	}

	reqBod, err := io.ReadAll(bodyReader)
	if err != nil {
		r.handlerReturnWithError(w, ErrPostBody, err)
		return
	}

	batchedEvents := make([]batchedEvent, 0)
	err = unmarshal(req, bytes.NewReader(reqBod), &batchedEvents)
	if err != nil {
		debugLog.WithField("error", err.Error()).WithField("request.url", req.URL).WithField("json_body", string(reqBod)).Logf("error parsing json")
		r.handlerReturnWithError(w, ErrJSONFailed, err)
		return
	}

	dataset, err := getDatasetFromRequest(req)
	if err != nil {
		r.handlerReturnWithError(w, ErrReqToEvent, err)
	}
	apiHost, err := r.Config.GetHoneycombAPI()
	if err != nil {
		r.handlerReturnWithError(w, ErrReqToEvent, err)
	}

	apiKey := req.Header.Get(types.APIKeyHeader)
	if apiKey == "" {
		apiKey = req.Header.Get(types.APIKeyHeaderShort)
	}

	// get environment name - will be empty for legacy keys
	environment, err := r.getEnvironmentName(apiKey)
	if err != nil {
		r.handlerReturnWithError(w, ErrReqToEvent, err)
	}

	batchedResponses := make([]*BatchResponse, 0, len(batchedEvents))
	for _, bev := range batchedEvents {
		ev := &types.Event{
			Context:     req.Context(),
			APIHost:     apiHost,
			APIKey:      apiKey,
			Dataset:     dataset,
			Environment: environment,
			SampleRate:  bev.getSampleRate(),
			Timestamp:   bev.getEventTime(),
			Data:        bev.Data,
		}

		err = r.processEvent(ev, reqID)

		var resp BatchResponse
		switch {
		case errors.Is(err, collect.ErrWouldBlock):
			resp.Status = http.StatusTooManyRequests
			resp.Error = err.Error()
		case err != nil:
			resp.Status = http.StatusBadRequest
			resp.Error = err.Error()
		default:
			resp.Status = http.StatusAccepted
		}
		batchedResponses = append(batchedResponses, &resp)
	}
	response, err := json.Marshal(batchedResponses)
	if err != nil {
		r.handlerReturnWithError(w, ErrJSONBuildFailed, err)
		return
	}
	w.Write(response)
}

func (router *Router) processOTLPRequest(
	ctx context.Context,
	batches []huskyotlp.Batch,
	apiKey string) error {

	var requestID types.RequestIDContextKey
	apiHost, err := router.Config.GetHoneycombAPI()
	if err != nil {
		router.Logger.Error().Logf("Unable to retrieve APIHost from config while processing OTLP batch")
		return err
	}

	// get environment name - will be empty for legacy keys
	environment, err := router.getEnvironmentName(apiKey)
	if err != nil {
		return nil
	}

	for _, batch := range batches {
		for _, ev := range batch.Events {
			event := &types.Event{
				Context:     ctx,
				APIHost:     apiHost,
				APIKey:      apiKey,
				Dataset:     batch.Dataset,
				Environment: environment,
				SampleRate:  uint(ev.SampleRate),
				Timestamp:   ev.Timestamp,
				Data:        ev.Attributes,
			}
			if err = router.processEvent(event, requestID); err != nil {
				router.Logger.Error().Logf("Error processing event: " + err.Error())
			}
		}
	}

	return nil
}

func (r *Router) processEvent(ev *types.Event, reqID interface{}) error {
	debugLog := r.iopLogger.Debug().
		WithField("request_id", reqID).
		WithString("api_host", ev.APIHost).
		WithString("dataset", ev.Dataset).
		WithString("environment", ev.Environment)

	// check if this is a probe from another refinery; if so, we should drop it
	if ev.Data["meta.refinery.probe"] != nil {
		debugLog.Logf("dropping probe")
		return nil
	}

	// extract trace ID
	var traceID string
	for _, traceIdFieldName := range r.Config.GetTraceIdFieldNames() {
		if trID, ok := ev.Data[traceIdFieldName]; ok {
			traceID = trID.(string)
			break
		}
	}
	if traceID == "" {
		// not part of a trace. send along upstream
		r.Metrics.Increment("incoming_router_nonspan")
		debugLog.WithString("api_host", ev.APIHost).
			WithString("dataset", ev.Dataset).
			Logf("sending non-trace event from batch")
		r.UpstreamTransmission.EnqueueEvent(ev)
		return nil
	}
	var spanID string
	for _, spanIdFieldName := range r.Config.GetSpanIdFieldNames() {
		if spID, ok := ev.Data[spanIdFieldName]; ok {
			spanID = spID.(string)
			break
		}
	}
	if spanID == "" {
		spanID = types.GenerateSpanID(traceID)
	}
	debugLog = debugLog.WithString("trace_id", traceID)

	var isRoot bool
	for _, parentIdFieldName := range r.Config.GetParentIdFieldNames() {
		_, isRoot = ev.Data[parentIdFieldName]
	}

	span := &types.Span{
		Event:   *ev,
		TraceID: traceID,
		ID:      spanID,
		IsRoot:  isRoot,
	}

	// we know we're a span, but we need to check if we're in Stress Relief mode;
	// if we are, then we hash the trace ID to determine if we should process it immediately
	// based on the hash and current stress levels.
	// The decision is only saved in the in-memory cache so that we can make the same decision
	// for all spans in the same trace.
	// If it's not a trace we should process immediately, we'll add it to the collector
	if r.Collector.Stressed() {
		processed, err := r.Collector.ProcessSpanImmediately(span)
		if err != nil {
			return err
		}
		if processed {
			return nil
		}
	}

	// we're supposed to handle it normally
	var err error
	err = r.Collector.AddSpan(span)
	if err != nil {
		r.Metrics.Increment("incoming_router_dropped")
		debugLog.Logf("Dropping span from batch, channel full")
		return err
	}

	r.Metrics.Increment("incoming_router_span")

	debugLog.WithField("source", "incoming").Logf("Accepting span from batch for collection into a trace")
	return nil
}

func (r *Router) getMaybeCompressedBody(req *http.Request) (io.Reader, error) {
	var reader io.Reader
	switch req.Header.Get("Content-Encoding") {
	case "gzip":
		gzipReader, err := gzip.NewReader(req.Body)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()

		buf := &bytes.Buffer{}
		if _, err := io.Copy(buf, gzipReader); err != nil {
			return nil, err
		}
		reader = buf
	case "zstd":
		zReader := <-r.zstdDecoders
		defer func(zReader *zstd.Decoder) {
			zReader.Reset(nil)
			r.zstdDecoders <- zReader
		}(zReader)

		err := zReader.Reset(req.Body)
		if err != nil {
			return nil, err
		}
		buf := &bytes.Buffer{}
		if _, err := io.Copy(buf, zReader); err != nil {
			return nil, err
		}

		reader = buf
	default:
		reader = req.Body
	}
	return reader, nil
}

type batchedEvent struct {
	Timestamp        string                 `json:"time"`
	MsgPackTimestamp *time.Time             `msgpack:"time,omitempty"`
	SampleRate       int64                  `json:"samplerate" msgpack:"samplerate"`
	Data             map[string]interface{} `json:"data" msgpack:"data"`
}

func (b *batchedEvent) getEventTime() time.Time {
	if b.MsgPackTimestamp != nil {
		return b.MsgPackTimestamp.UTC()
	}

	return getEventTime(b.Timestamp)
}

func (b *batchedEvent) getSampleRate() uint {
	if b.SampleRate == 0 {
		return defaultSampleRate
	}
	return uint(b.SampleRate)
}

// getEventTime tries to guess the time format in our time header!
// Allowable options are
// * RFC3339Nano
// * RFC3339
// * Unix Epoch time (integer seconds since 1970, eg 1535589382)
// * High resolution unix epoch time (eg 'unixmillis' 1535589382641)
// * High resolution unix epoch time as a float (eg 1535589382.641)
func getEventTime(etHeader string) time.Time {
	var eventTime time.Time
	if etHeader != "" {
		// Great, they sent us a time header. let's try and parse it.
		// RFC3339Nano is the default that we send from all our SDKs
		eventTime, _ = time.Parse(time.RFC3339Nano, etHeader)
		if eventTime.IsZero() {
			// the default didn't catch it, let's try a few other things
			// is it all numeric? then try unix epoch times
			epochInt, err := strconv.ParseInt(etHeader, 0, 64)
			if err == nil {
				// it might be seconds or it might be milliseconds! Who can know!
				// 10-digit numbers are seconds, 13-digit milliseconds, 16 microseconds
				if len(etHeader) == 10 {
					eventTime = time.Unix(epochInt, 0)
				} else if len(etHeader) > 10 {
					// turn it into seconds and fractional seconds
					fractionalTime := etHeader[:10] + "." + etHeader[10:]
					// then chop it into the int part and the fractional part
					if epochFloat, err := strconv.ParseFloat(fractionalTime, 64); err == nil {
						sec, dec := math.Modf(epochFloat)
						eventTime = time.Unix(int64(sec), int64(dec*(1e9)))
					}

				}
			} else {
				epochFloat, err := strconv.ParseFloat(etHeader, 64)
				if err == nil {
					sec, dec := math.Modf(epochFloat)
					eventTime = time.Unix(int64(sec), int64(dec*(1e9)))
				}
			}
		}
	}
	return eventTime.UTC()
}

func makeDecoders(num int) (chan *zstd.Decoder, error) {
	zstdDecoders := make(chan *zstd.Decoder, num)
	for i := 0; i < num; i++ {
		zReader, err := zstd.NewReader(
			nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxMemory(8*1024*1024),
		)
		if err != nil {
			return nil, err
		}
		zstdDecoders <- zReader
	}
	return zstdDecoders, nil
}

func unmarshal(r *http.Request, data io.Reader, v interface{}) error {
	switch r.Header.Get("Content-Type") {
	case "application/x-msgpack", "application/msgpack":
		decoder := msgpack.NewDecoder(data)
		decoder.UseLooseInterfaceDecoding(true)
		return decoder.Decode(v)
	default:
		return jsoniter.NewDecoder(data).Decode(v)
	}
}

func getAPIKeyAndDatasetFromMetadata(md metadata.MD) (apiKey string, dataset string) {
	apiKey = getFirstValueFromMetadata(types.APIKeyHeader, md)
	if apiKey == "" {
		apiKey = getFirstValueFromMetadata(types.APIKeyHeaderShort, md)
	}
	dataset = getFirstValueFromMetadata(types.DatasetHeader, md)

	return apiKey, dataset
}

// getFirstValueFromMetadata returns the first value of a metadata entry using a
// case-insensitive key
func getFirstValueFromMetadata(key string, md metadata.MD) string {
	if values := md.Get(key); len(values) > 0 {
		return values[0]
	}
	return ""
}

type environmentCache struct {
	mutex sync.RWMutex
	items map[string]*cacheItem
	ttl   time.Duration
	getFn func(string) (string, error)
}

func (r *Router) SetEnvironmentCache(ttl time.Duration, getFn func(string) (string, error)) {
	r.environmentCache = newEnvironmentCache(ttl, getFn)
}

func newEnvironmentCache(ttl time.Duration, getFn func(string) (string, error)) *environmentCache {
	return &environmentCache{
		items: make(map[string]*cacheItem),
		ttl:   ttl,
		getFn: getFn,
	}
}

type cacheItem struct {
	expiresAt time.Time
	value     string
}

// get queries the cached items, returning cache hits that have not expired.
// Cache missed use the configured getFn to populate the cache.
func (c *environmentCache) get(key string) (string, error) {
	var val string
	// get read lock so that we don't attempt to read from the map
	// while another routine has a write lock and is actively writing
	// to the map.
	c.mutex.RLock()
	if item, ok := c.items[key]; ok {
		if time.Now().Before(item.expiresAt) {
			val = item.value
		}
	}
	c.mutex.RUnlock()
	if val != "" {
		return val, nil
	}

	// get write lock early so we don't execute getFn in parallel so the
	// the result will be cached before the next lock is acquired to prevent
	// subsequent calls to getFn for the same key
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// check if the cache has been populated while waiting for a write lock
	if item, ok := c.items[key]; ok {
		if time.Now().Before(item.expiresAt) {
			return item.value, nil
		}
	}

	val, err := c.getFn(key)
	if err != nil {
		return "", err
	}

	c.addItem(key, val, c.ttl)
	return val, nil
}

// addItem create a new cache entry in the environment cache.
// This is not thread-safe, and should only be used in tests
func (c *environmentCache) addItem(key string, value string, ttl time.Duration) {
	c.items[key] = &cacheItem{
		expiresAt: time.Now().Add(ttl),
		value:     value,
	}
}

type TeamInfo struct {
	Slug string `json:"slug"`
}

type EnvironmentInfo struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type AuthInfo struct {
	APIKeyAccess map[string]bool `json:"api_key_access"`
	Team         TeamInfo        `json:"team"`
	Environment  EnvironmentInfo `json:"environment"`
}

func (r *Router) getEnvironmentName(apiKey string) (string, error) {
	if apiKey == "" || types.IsLegacyAPIKey(apiKey) {
		return "", nil
	}

	env, err := r.environmentCache.get(apiKey)
	if err != nil {
		return "", err
	}
	return env, nil
}

func (r *Router) lookupEnvironment(apiKey string) (string, error) {
	apiEndpoint := r.Config.GetHoneycombAPI()
	authURL, err := url.Parse(apiEndpoint)
	if err != nil {
		return "", fmt.Errorf("failed to parse Honeycomb API URL config value. %w", err)
	}

	authURL.Path = "/1/auth"
	req, err := http.NewRequest("GET", authURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("failed to create AuthInfo request. %w", err)
	}

	req.Header.Set("x-Honeycomb-team", apiKey)

	r.Logger.Debug().WithString("endpoint", authURL.String()).Logf("Attempting to get environment name using API key")
	resp, err := r.proxyClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed sending AuthInfo request to Honeycomb API. %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "", fmt.Errorf("received 401 response for AuthInfo request from Honeycomb API - check your API key")
	case resp.StatusCode > 299:
		return "", fmt.Errorf("received %d response for AuthInfo request from Honeycomb API", resp.StatusCode)
	}

	authinfo := AuthInfo{}
	if err := json.NewDecoder(resp.Body).Decode(&authinfo); err != nil {
		return "", fmt.Errorf("failed to JSON decode of AuthInfo response from Honeycomb API")
	}
	r.Logger.Debug().WithString("environment", authinfo.Environment.Name).Logf("Got environment")
	return authinfo.Environment.Name, nil
}

// healthchecker is a goroutine that periodically checks the health of the system and updates the grpc health server
func (r *Router) healthchecker() {
	const (
		system      = "" // empty string represents the generic health of the whole system (corresponds to "ready")
		systemReady = "ready"
		systemAlive = "alive"
	)
	r.iopLogger.Debug().Logf("running grpc health monitor")

	setStatus := func(svc string, stat bool) {
		if stat {
			r.hsrv.SetServingStatus(svc, grpc_health_v1.HealthCheckResponse_SERVING)
		} else {
			r.hsrv.SetServingStatus(svc, grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		}
	}

	go func() {
		// TODO: Does this time need to be configurable?
		watchticker := time.NewTicker(3 * time.Second)
		defer watchticker.Stop()
		for {
			select {
			case <-watchticker.C:
				alive := r.Health.IsAlive()
				ready := r.Health.IsReady()

				// we can just update everything because the grpc health server will only send updates if the status changes
				setStatus(systemReady, ready)
				setStatus(systemAlive, alive)
				setStatus(system, ready && alive)
			case <-r.donech:
				return
			}
		}
	}()
}

// AddOTLPMuxxer adds muxxer for OTLP requests
func (r *Router) AddOTLPMuxxer(muxxer *mux.Router) {
	// require an auth header for OTLP requests
	otlpMuxxer := muxxer.PathPrefix("/v1/").Methods("POST").Subrouter()

	// handle OTLP trace requests
	otlpMuxxer.HandleFunc("/traces", r.postOTLPTrace).Name("otlp_traces")
	otlpMuxxer.HandleFunc("/traces/", r.postOTLPTrace).Name("otlp_traces")

	// handle OTLP logs requests
	otlpMuxxer.HandleFunc("/logs", r.postOTLPLogs).Name("otlp_logs")
	otlpMuxxer.HandleFunc("/logs/", r.postOTLPLogs).Name("otlp_logs")
}

func getDatasetFromRequest(req *http.Request) (string, error) {
	dataset := mux.Vars(req)["datasetName"]
	if dataset == "" {
		return "", fmt.Errorf("missing dataset name")
	}
	dataset, err := url.PathUnescape(dataset)
	if err != nil {
		return "", err
	}
	return dataset, nil
}
