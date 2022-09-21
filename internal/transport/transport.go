package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/alpine-hodler/gidari/internal/storage"
	"github.com/alpine-hodler/gidari/internal/web"
	"github.com/alpine-hodler/gidari/internal/web/auth"
	"github.com/alpine-hodler/gidari/proto"
	"github.com/alpine-hodler/gidari/repository"
	"github.com/alpine-hodler/gidari/tools"
	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

var (
	// ErrFetchingTimeseriesChunks is returned when the timeseries is unable to fetch the chunks.
	ErrFetchingTimeseriesChunks = fmt.Errorf("failed to fetch timeseries chunks")

	// ErrInvalidRateLimit is returned when the rate limit configuration is invalid.
	ErrInvalidRateLimit = fmt.Errorf("invalid rate limit configuration")

	// ErrMissingConfigField is returned when a configuration field is missing.
	ErrMissingConfigField = fmt.Errorf("missing config field")

	// ErrMissingRateLimitField is returned when the rate limit configuration is missing a field.
	ErrMissingRateLimitField = fmt.Errorf("missing rate limit field")

	// ErrMissingTimeseriesField is returned when the timeseries is missing from the configuration.
	ErrMissingTimeseriesField = fmt.Errorf("missing timeseries field")

	// ErrSettingTimeseriesChunks is returned when the timeseries is unable to set the chunks.
	ErrSettingTimeseriesChunks = fmt.Errorf("failed to set timeseries chunks")

	// ErrUnableToParse is returned with a parser is unable to parse the data.
	ErrUnableToParse = fmt.Errorf("unable to parse")
)

// MissingConfigFieldError is returned when a configuration field is missing.
func MissingConfigFieldError(field string) error {
	return fmt.Errorf("%w: %s", ErrMissingConfigField, field)
}

// MissingRateLimitFieldError is returned when the rate limit configuration is missing a field.
func MissingRateLimitFieldError(field string) error {
	return fmt.Errorf("%w: %s", ErrMissingRateLimitField, field)
}

// MissingTimeseriesFieldError is returned when the timeseries is missing from the configuration.
func MissingTimeseriesFieldError(field string) error {
	return fmt.Errorf("%w: %s", ErrMissingTimeseriesField, field)
}

// UnableToParseError is returned when a parser is unable to parse the data.
func UnableToParseError(name string) error {
	return fmt.Errorf("%s %w", name, ErrUnableToParse)
}

// WrapRepositoryError will wrap an error from the repository with a message.
func WrapRepositoryError(err error) error {
	return fmt.Errorf("repository: %w", err)
}

// WrapWebError will wrap an error from the web package with a message.
func WrapWebError(err error) error {
	return fmt.Errorf("web: %w", err)
}

// APIKey is one method of HTTP(s) transport that requires a passphrase, key, and secret.
type APIKey struct {
	Passphrase string `yaml:"passphrase"`
	Key        string `yaml:"key"`
	Secret     string `yaml:"secret"`
}

// Auth2 is a struct that contains the authentication data for a web API that uses OAuth2.
type Auth2 struct {
	Bearer string `yaml:"bearer"`
}

// Authentication is the credential information to be used to construct an HTTP(s) transport for accessing the API.
type Authentication struct {
	APIKey *APIKey `yaml:"apiKey"`
	Auth2  *Auth2  `yaml:"auth2"`
}

// timeseries is a struct that contains the information needed to query a web API for timeseries data.
type timeseries struct {
	StartName string `yaml:"startName"`
	EndName   string `yaml:"endName"`

	// Period is the size of each chunk in seconds for which we can query the API. Some API will not allow us to
	// query all data within the start and end range.
	Period int32 `yaml:"period"`

	// Layout is the time layout for parsing the "Start" and "End" values into "time.Time". The default is assumed
	// to be RFC3339.
	Layout *string `yaml:"layout"`

	// chunks are the time ranges for which we can query the API. These are broken up into pieces for API requests
	// that only return a limited number of results.
	chunks [][2]time.Time
}

// setChunks will attempt to use the query string of a URL to partition the timeseries into "chunks" of time for queying
// a web API.
func (ts *timeseries) setChunks(url *url.URL) error {
	// If layout is not set, then default it to be RFC3339
	if ts.Layout == nil {
		str := time.RFC3339
		ts.Layout = &str
	}

	query := url.Query()
	startSlice := query[ts.StartName]
	if len(startSlice) != 1 {
		return MissingTimeseriesFieldError("startName")
	}

	start, err := time.Parse(*ts.Layout, startSlice[0])
	if err != nil {
		return UnableToParseError("startTime")
	}

	endSlice := query[ts.EndName]
	if len(endSlice) != 1 {
		return MissingTimeseriesFieldError("endName")
	}

	end, err := time.Parse(*ts.Layout, endSlice[0])
	if err != nil {
		return UnableToParseError("endTime")
	}

	for start.Before(end) {
		next := start.Add(time.Second * time.Duration(ts.Period))
		if next.Before(end) {
			ts.chunks = append(ts.chunks, [2]time.Time{start, next})
		} else {
			ts.chunks = append(ts.chunks, [2]time.Time{start, end})
		}
		start = next
	}
	return nil
}

// Request is the information needed to query the web API for data to transport.
type Request struct {
	// Method is the HTTP(s) method used to construct the http request to fetch data for storage.
	Method string `yaml:"method"`

	// Endpoint is the fragment of the URL that will be used to request data from the API. This value can include
	// query parameters.
	Endpoint string `yaml:"endpoint"`

	// RateLimitBurstCap represents the number of requests that can be made per second to the endpoint. The
	// value of this should come from the documentation in the underlying API.
	RateLimitBurstCap int `yaml:"ratelimit"`

	// Query represent the query params to apply to the URL generated by the request.
	Query map[string]string

	// Timeseries indicates that the underlying data should be queries as a time series. This means that the
	Timeseries *timeseries `yaml:"timeseries"`

	// Table is the name of the table/collection to insert the data fetched from the web API.
	Table *string
}

// RateLimitConfig is the data needed for constructing a rate limit for the HTTP requests.
type RateLimitConfig struct {
	// Burst represents the number of requests that we limit over a period frequency.
	Burst *int `yaml:"burst"`

	// Period is the number of times to allow a burst per second.
	Period *time.Duration `yaml:"period"`
}

func (rl RateLimitConfig) validate() error {
	if rl.Burst == nil {
		return MissingRateLimitFieldError("burst")
	}
	if rl.Period == nil {
		return MissingRateLimitFieldError("period")
	}
	return nil
}

// Config is the configuration used to query data from the web using HTTP requests and storing that data using
// the repositories defined by the "DNSList".
type Config struct {
	URL             string           `yaml:"url"`
	Authentication  Authentication   `yaml:"authentication"`
	DNSList         []string         `yaml:"dnsList"`
	Requests        []*Request       `yaml:"requests"`
	RateLimitConfig *RateLimitConfig `yaml:"rateLimit"`

	Logger   *logrus.Logger
	Truncate bool
}

// connect will attempt to connect to the web API client. Since there are multiple ways to build a transport given the
// authentication data, this method will exhaust every transport option in the "Authentication" struct.
func (cfg *Config) connect(ctx context.Context) (*web.Client, error) {
	if apiKey := cfg.Authentication.APIKey; apiKey != nil {
		client, err := web.NewClient(ctx, auth.NewAPIKey().
			SetURL(cfg.URL).
			SetKey(apiKey.Key).
			SetPassphrase(apiKey.Passphrase).
			SetSecret(apiKey.Secret))
		if err != nil {
			return nil, WrapWebError(web.FailedToCreateClientError(err))
		}
		return client, nil
	}
	if apiKey := cfg.Authentication.Auth2; apiKey != nil {
		client, err := web.NewClient(ctx, auth.NewAuth2().SetBearer(apiKey.Bearer).SetURL(cfg.URL))
		if err != nil {
			return nil, WrapWebError(web.FailedToCreateClientError(err))
		}
		return client, nil
	}
	return nil, nil
}

// repos will return a slice of generic repositories along with associated transaction instances.
func (cfg *Config) repos(ctx context.Context) ([]repository.Generic, error) {
	repos := []repository.Generic{}
	for _, dns := range cfg.DNSList {
		repo, err := repository.NewTx(ctx, dns)
		if err != nil {
			return nil, WrapRepositoryError(repository.FailedToCreateRepositoryError(err))
		}
		logInfo := tools.LogFormatter{
			Msg: fmt.Sprintf("created repository for %q", dns),
		}
		cfg.Logger.Info(logInfo.String())

		repos = append(repos, repo)
	}
	return repos, nil
}

// validate will ensure that the configuration is valid for querying the web API.
func (cfg *Config) validate() error {
	if cfg.RateLimitConfig == nil {
		return MissingConfigFieldError("rateLimit")
	}

	if err := cfg.RateLimitConfig.validate(); err != nil {
		return ErrInvalidRateLimit
	}
	return nil
}

// newFetchConfig constructs a new HTTP request.
func newFetchConfig(_ context.Context, cfg *Config, req *Request, client *web.Client,
	rateLimiter *rate.Limiter) (*web.FetchConfig, error) {
	rawURL, err := url.JoinPath(cfg.URL, req.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("error joining url %q to endpoint %q: %w", cfg.URL, req.Endpoint, err)
	}

	uri, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("error parsing raw url %q: %w", rawURL, err)
	}

	// Apply the query parameters if they are on the request.
	if req.Query != nil {
		query := uri.Query()
		for n, v := range req.Query {
			query.Set(n, v)
		}
		uri.RawQuery = query.Encode()
	}

	webcfg := &web.FetchConfig{
		C:           client,
		Method:      req.Method,
		URL:         uri,
		RateLimiter: rateLimiter,
	}

	return webcfg, nil
}

type repoJob struct {
	req   http.Request
	b     []byte
	table *string
}

type repoConfig struct {
	repos    []repository.Generic
	jobs     <-chan *repoJob
	done     chan bool
	logger   *logrus.Logger
	truncate bool
}

func repositoryWorker(_ context.Context, workerID int, cfg *repoConfig) {
	for job := range cfg.jobs {
		req, err := RepositoryEncoders.Lookup(job.req.URL).Encode(job.req, job.b)
		if err != nil {
			cfg.logger.Fatalf("error encoding response data: %v", err)
		}

		// If a table is defined for the job, then replace the table name in the raw data.
		if table := job.table; table != nil {
			req.Table = *table
		}

		for _, repo := range cfg.repos {
			txfn := func(sctx context.Context, repo repository.Generic) error {
				start := time.Now()
				rsp, err := repo.Upsert(sctx, req)
				if err != nil {
					cfg.logger.Fatalf("error upserting data: %v", err)
					return fmt.Errorf("error upserting data: %w", err)
				}
				rt := repo.Type()
				msg := fmt.Sprintf("partial upsert completed: %s.%s", storage.Scheme(rt), req.Table)
				logInfo := tools.LogFormatter{
					WorkerID:      workerID,
					WorkerName:    "repository",
					Duration:      time.Since(start),
					Msg:           msg,
					UpsertedCount: rsp.UpsertedCount,
					MatchedCount:  rsp.MatchedCount,
				}
				cfg.logger.Infof(logInfo.String())
				return nil
			}
			// Put the data onto the transaction channel for storage.
			repo.Transact(txfn)
		}
		cfg.done <- true
	}
}

// flattenedRequest contains all of the request information to create a web job. The number of flattened request
// for an operation should be 1-1 with the number of requests to the web API.
type flattenedRequest struct {
	fetchConfig *web.FetchConfig
	table       *string
}

type webWorkerJob struct {
	*flattenedRequest
	repoJobs chan<- *repoJob
	client   *web.Client
	logger   *logrus.Logger
}

func webWorker(ctx context.Context, workerID int, jobs <-chan *webWorkerJob) {
	for job := range jobs {
		start := time.Now()

		rsp, err := web.Fetch(ctx, job.fetchConfig)
		if err != nil {
			job.logger.Fatal(err)
		}

		bytes, err := io.ReadAll(rsp.Body)
		if err != nil {
			job.logger.Fatal(err)
		}

		job.repoJobs <- &repoJob{b: bytes, req: *rsp.Request, table: job.table}

		logInfo := tools.LogFormatter{
			WorkerID:   workerID,
			WorkerName: "web",
			Duration:   time.Since(start),
			Msg:        fmt.Sprintf("web request completed: %s", rsp.Request.URL.Path),
		}
		job.logger.Infof(logInfo.String())
	}
}

// Upsert will use the configuration file to upsert data from the
//
// For each DNS entry in the configuration file, a repository will be created and used to upsert data. For each
// repository, a transaction will be created and used to upsert data. The transaction will be committed at the end
// of the upsert operation. If the transaction fails, the transaction will be rolled back. Note that it is possible
// for some repository transactions to succeed and others to fail.
func Upsert(ctx context.Context, cfg *Config) error {
	start := time.Now()

	err := cfg.validate()
	if err != nil {
		return err
	}

	client, err := cfg.connect(ctx)
	if err != nil {
		return fmt.Errorf("unable to connect to client: %w", err)
	}

	cfg.Logger.Info(tools.LogFormatter{Msg: fmt.Sprintf("connection establed: %s", cfg.URL)}.String())

	repos, err := cfg.repos(ctx)
	if err != nil {
		return err
	}

	// Convert the RateLimitConfig.Period to seconds.
	rateLimitPeriodS := *cfg.RateLimitConfig.Period / time.Second

	// create a rate limiter to pass to all "flattenedRequest". This has to be defined outside of the scope of
	// individual "flattenedRequest"s so that they all share the same rate limiter, even concurrent requests to
	// different endpoints could cause a rate limit error on a web API.
	rateLimiter := rate.NewLimiter(rate.Every(rateLimitPeriodS), *cfg.RateLimitConfig.Burst)

	// Get all of the fetch configurations needed to process the upsert.
	var flattenedRequests []*flattenedRequest
	truncateRequest := new(proto.TruncateRequest)

	for _, req := range cfg.Requests {
		// Get is the implicit default method.
		if req.Method == "" {
			req.Method = http.MethodGet
		}

		fetchConfig, err := newFetchConfig(ctx, cfg, req, client, rateLimiter)
		if err != nil {
			return err
		}

		if timeseries := req.Timeseries; timeseries != nil {
			xurl := fetchConfig.URL
			err = timeseries.setChunks(xurl)

			if err != nil {
				return ErrSettingTimeseriesChunks
			}
			for _, chunk := range timeseries.chunks {
				// copy the request and update it to reflect the partitioned timeseries
				chunkReq := req
				chunkReq.Query[timeseries.StartName] = chunk[0].Format(*timeseries.Layout)
				chunkReq.Query[timeseries.EndName] = chunk[1].Format(*timeseries.Layout)

				chunkedFetchConfig, err := newFetchConfig(ctx, cfg, chunkReq, client, rateLimiter)
				if err != nil {
					return ErrFetchingTimeseriesChunks
				}

				flattenedRequests = append(flattenedRequests, &flattenedRequest{
					fetchConfig: chunkedFetchConfig,
					table:       req.Table,
				})
			}
		} else {
			flattenedRequests = append(flattenedRequests, &flattenedRequest{
				fetchConfig: fetchConfig,
				table:       req.Table,
			})
		}

		// Add the table to the list of tables to truncate.
		if table := req.Table; table != nil {
			truncateRequest.Tables = append(truncateRequest.Tables, *table)
		}
	}

	// Truncate the tables before upserting data.
	if cfg.Truncate {
		for _, repo := range repos {
			start := time.Now()

			_, err := repo.Truncate(ctx, truncateRequest)
			if err != nil {
				return fmt.Errorf("unable to truncate tables: %w", err)
			}

			rt := repo.Type()
			tables := strings.Join(truncateRequest.Tables, ", ")
			msg := fmt.Sprintf("truncated tables on %q: %v", storage.Scheme(rt), tables)
			logInfo := tools.LogFormatter{
				Duration: time.Since(start),
				Msg:      msg,
			}
			cfg.Logger.Infof(logInfo.String())
		}
	}

	// repoJobs is a channel that will be used to pass jobs to the repository workers. The repository workers will
	// be responsible for upserting the data into the database.
	repoJobCh := make(chan *repoJob, len(flattenedRequests)*len(repos))

	repoWorkerCfg := &repoConfig{
		repos:    repos,
		logger:   cfg.Logger,
		done:     make(chan bool, len(flattenedRequests)),
		jobs:     repoJobCh,
		truncate: cfg.Truncate,
	}

	// Start the repository workers.
	for id := 1; id <= runtime.NumCPU(); id++ {
		go repositoryWorker(ctx, id, repoWorkerCfg)
	}

	cfg.Logger.Info(tools.LogFormatter{Msg: "repository workers started"}.String())

	webWorkerJobs := make(chan *webWorkerJob, len(cfg.Requests))

	// Start the same number of web workers as the cores on the machine.
	for id := 1; id <= runtime.NumCPU(); id++ {
		go webWorker(ctx, id, webWorkerJobs)
	}

	cfg.Logger.Info(tools.LogFormatter{Msg: "web workers started"}.String())

	// Enqueue the worker jobs
	for _, req := range flattenedRequests {
		webWorkerJobs <- &webWorkerJob{
			flattenedRequest: req,
			repoJobs:         repoJobCh,
			client:           client,
			logger:           cfg.Logger,
		}
	}

	cfg.Logger.Info(tools.LogFormatter{Msg: "web worker jobs enqueued"}.String())

	// Wait for all of the data to flush.
	for a := 1; a <= len(flattenedRequests); a++ {
		<-repoWorkerCfg.done
	}

	// Commit the transactions and check for errors.
	for _, repo := range repos {
		if err := repo.Commit(); err != nil {
			return fmt.Errorf("unable to commit transaction: %w", err)
		}
	}

	logInfo := tools.LogFormatter{
		Duration: time.Since(start),
		Msg:      "upsert completed",
	}
	cfg.Logger.Info(logInfo.String())
	return nil
}
