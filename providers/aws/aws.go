// Package aws provides mamori value providers backed by Amazon Web Services.
//
// It registers two schemes, each handled by its own provider type:
//
//   - aws-sm://<secret-id>[#json-key]      AWS Secrets Manager  (SMProvider)
//   - aws-ps://<parameter-name>[#json-key] SSM Parameter Store  (PSProvider)
//
// Both providers create their underlying AWS SDK client lazily on first use,
// using the default AWS credential chain (environment, shared config/profile,
// EC2/ECS/EKS role, SSO, ...). The AWS region is taken from the same ambient
// configuration unless overridden with WithRegion.
//
// A #json-key fragment selects a single field from a JSON object payload using
// mamori.SelectKey, identically to every other mamori provider. Secrets Manager
// values are always marked Sensitive; Parameter Store values are marked
// Sensitive only when the parameter is a SecureString.
//
// Neither backend has native change notification, so neither provider
// implements WatchableProvider - mamori polls them. Both implement
// mamori.BatchProvider (BatchGetSecretValue / GetParameters) so mamori can
// resolve many refs in a single API call.
//
// Usage with ambient credentials is automatic via the registered providers.
// Callers who need explicit configuration use:
//
//	cfg, _ := mamori.Load[Config](ctx,
//	    mamori.WithProvider(aws.NewSecretsManager(aws.WithRegion("us-east-1"))),
//	    mamori.WithProvider(aws.NewParameterStore(aws.WithRegion("us-east-1"))),
//	)
package aws

import (
	"context"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/xavidop/mamori"
)

// options holds the construction-time configuration shared by both providers.
type options struct {
	region string
}

// Option customizes a provider constructed with NewSecretsManager or
// NewParameterStore.
type Option func(*options)

// WithRegion pins the AWS region for the provider's client. When unset, the
// region is resolved from the ambient AWS configuration (AWS_REGION,
// AWS_DEFAULT_REGION, the shared config file, or instance metadata).
func WithRegion(region string) Option {
	return func(o *options) { o.region = region }
}

// loadConfig builds an aws.Config from the default credential chain, applying
// any provider options. It honors ctx for the (network-capable) credential and
// region resolution steps.
func loadConfig(ctx context.Context, o options) (awssdk.Config, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if o.region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(o.region))
	}
	return awsconfig.LoadDefaultConfig(ctx, loadOpts...)
}

func newOptions(opts []Option) options {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// init registers a lazily-initialized instance of each provider so that
// aws-sm:// and aws-ps:// refs resolve out of the box under ambient AWS
// credentials. Two distinct types are registered because mamori.Register
// panics on a duplicate scheme.
func init() {
	mamori.Register(NewSecretsManager())
	mamori.Register(NewParameterStore())
}
