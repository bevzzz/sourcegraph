package httpapi

import (
	"context"
	"net/http"

	"github.com/Khan/genqlient/graphql"
	"github.com/gorilla/mux"
	"github.com/sourcegraph/log"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sourcegraph/sourcegraph/cmd/cody-gateway/shared/config"

	"github.com/sourcegraph/sourcegraph/cmd/cody-gateway/internal/auth"
	"github.com/sourcegraph/sourcegraph/cmd/cody-gateway/internal/events"
	"github.com/sourcegraph/sourcegraph/cmd/cody-gateway/internal/httpapi/attribution"
	"github.com/sourcegraph/sourcegraph/cmd/cody-gateway/internal/httpapi/completions"
	"github.com/sourcegraph/sourcegraph/cmd/cody-gateway/internal/httpapi/embeddings"
	"github.com/sourcegraph/sourcegraph/cmd/cody-gateway/internal/httpapi/featurelimiter"
	"github.com/sourcegraph/sourcegraph/cmd/cody-gateway/internal/httpapi/requestlogger"
	"github.com/sourcegraph/sourcegraph/cmd/cody-gateway/internal/limiter"
	"github.com/sourcegraph/sourcegraph/cmd/cody-gateway/internal/notify"
	"github.com/sourcegraph/sourcegraph/internal/httpcli"
	"github.com/sourcegraph/sourcegraph/internal/instrumentation"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

type Config struct {
	RateLimitNotifier           notify.RateLimitNotifier
	Anthropic                   config.AnthropicConfig
	OpenAI                      config.OpenAIConfig
	Fireworks                   config.FireworksConfig
	EmbeddingsAllowedModels     []string
	AutoFlushStreamingResponses bool
	EnableAttributionSearch     bool
}

var meter = otel.GetMeterProvider().Meter("cody-gateway/internal/httpapi")

var (
	attributesAnthropicCompletions = newMetricAttributes("anthropic", "completions")
	attributesOpenAICompletions    = newMetricAttributes("openai", "completions")
	attributesOpenAIEmbeddings     = newMetricAttributes("openai", "embeddings")
	attributesFireworksCompletions = newMetricAttributes("fireworks", "completions")
)

func NewHandler(
	logger log.Logger,
	eventLogger events.Logger,
	rs limiter.RedisStore,
	httpClient httpcli.Doer,
	authr *auth.Authenticator,
	promptRecorder completions.PromptRecorder,
	config *Config,
	dotcomClient graphql.Client,
) (http.Handler, error) {
	// Initialize metrics
	counter, err := meter.Int64UpDownCounter("cody-gateway.concurrent_upstream_requests",
		metric.WithDescription("number of concurrent active requests for upstream services"))
	if err != nil {
		return nil, errors.Wrap(err, "init metric 'concurrent_upstream_requests'")
	}
	r := mux.NewRouter()

	// V1 service routes
	v1router := r.PathPrefix("/v1").Subrouter()

	if config.Anthropic.AccessToken != "" {
		anthropicHandler, err := completions.NewAnthropicHandler(
			logger,
			eventLogger,
			rs,
			config.RateLimitNotifier,
			httpClient,
			config.Anthropic,
			promptRecorder,

			config.AutoFlushStreamingResponses,
		)
		if err != nil {
			return nil, errors.Wrap(err, "init Anthropic handler")
		}

		v1router.Path("/completions/anthropic").Methods(http.MethodPost).Handler(
			instrumentation.HTTPMiddleware("v1.completions.anthropic",
				gaugeHandler(
					counter,
					attributesAnthropicCompletions,
					authr.Middleware(
						requestlogger.Middleware(
							logger,
							anthropicHandler,
						),
					),
				),
				otelhttp.WithPublicEndpoint(),
			),
		)
	}
	if config.OpenAI.AccessToken != "" {
		v1router.Path("/completions/openai").Methods(http.MethodPost).Handler(
			instrumentation.HTTPMiddleware("v1.completions.openai",
				gaugeHandler(
					counter,
					attributesOpenAICompletions,
					authr.Middleware(
						requestlogger.Middleware(
							logger,
							completions.NewOpenAIHandler(
								logger,
								eventLogger,
								rs,
								config.RateLimitNotifier,
								httpClient,
								config.OpenAI,
								config.AutoFlushStreamingResponses,
							),
						),
					),
				),
				otelhttp.WithPublicEndpoint(),
			),
		)

		v1router.Path("/embeddings/models").Methods(http.MethodGet).Handler(
			instrumentation.HTTPMiddleware("v1.embeddings.models",
				authr.Middleware(
					requestlogger.Middleware(
						logger,
						embeddings.NewListHandler(),
					),
				),
				otelhttp.WithPublicEndpoint(),
			),
		)

		v1router.Path("/embeddings").Methods(http.MethodPost).Handler(
			instrumentation.HTTPMiddleware("v1.embeddings",
				gaugeHandler(
					counter,
					// TODO - if embeddings.ModelFactoryMap includes more than
					// just OpenAI we might need to move how we count concurrent
					// requests into the handler, instead of assuming we are
					// counting OpenAI requests
					attributesOpenAIEmbeddings,
					authr.Middleware(
						requestlogger.Middleware(
							logger,
							embeddings.NewHandler(
								logger,
								eventLogger,
								rs,
								config.RateLimitNotifier,
								embeddings.ModelFactoryMap{
									embeddings.ModelNameOpenAIAda: embeddings.NewOpenAIClient(httpClient, config.OpenAI.AccessToken),
								},
								config.EmbeddingsAllowedModels,
							),
						),
					),
				),
				otelhttp.WithPublicEndpoint(),
			),
		)
	}
	if config.Fireworks.AccessToken != "" {
		v1router.Path("/completions/fireworks").Methods(http.MethodPost).Handler(
			instrumentation.HTTPMiddleware("v1.completions.fireworks",
				gaugeHandler(
					counter,
					attributesFireworksCompletions,
					authr.Middleware(
						requestlogger.Middleware(
							logger,
							completions.NewFireworksHandler(
								logger,
								eventLogger,
								rs,
								config.RateLimitNotifier,
								httpClient,
								config.Fireworks,
								config.AutoFlushStreamingResponses,
							),
						),
					),
				),
				otelhttp.WithPublicEndpoint(),
			),
		)
	}

	// Register a route where actors can retrieve their current rate limit state.
	v1router.Path("/limits").Methods(http.MethodGet).Handler(
		instrumentation.HTTPMiddleware("v1.limits",
			authr.Middleware(
				requestlogger.Middleware(
					logger,
					featurelimiter.ListLimitsHandler(logger, rs),
				),
			),
			otelhttp.WithPublicEndpoint(),
		),
	)
	// Register a route where actors can refresh their rate limit state.
	v1router.Path("/limits/refresh").Methods(http.MethodPost).Handler(
		instrumentation.HTTPMiddleware("v1.limits",
			authr.Middleware(
				requestlogger.Middleware(
					logger,
					featurelimiter.RefreshLimitsHandler(logger),
				),
			),
			otelhttp.WithPublicEndpoint(),
		),
	)

	var attributionClient graphql.Client
	if config.EnableAttributionSearch {
		attributionClient = dotcomClient
	}
	v1router.Path("/attribution").Methods(http.MethodPost).Handler(
		instrumentation.HTTPMiddleware("v1.attribution",
			authr.Middleware(
				attribution.NewHandler(attributionClient, logger),
			),
			otelhttp.WithPublicEndpoint(),
		),
	)
	return r, nil
}

func newMetricAttributes(provider string, feature string) attribute.Set {
	return attribute.NewSet(
		attribute.String("provider", provider),
		attribute.String("feature", feature))
}

// gaugeHandler increments gauge when handling the request and decrements it
// upon completion.
func gaugeHandler(counter metric.Int64UpDownCounter, attrs attribute.Set, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(r.Context(), 1, metric.WithAttributeSet(attrs))
		handler.ServeHTTP(w, r)
		// Background context when done, since request may be cancelled.
		counter.Add(context.Background(), -1, metric.WithAttributeSet(attrs))
	})
}
