package releaseset

import (
	"context"

	runtimeproto "go.putnami.dev/protocol/runtime"
)

// publishedImagesFileEnv is the private framework-to-reserved-provider file.
const publishedImagesFileEnv = runtimeproto.ReleaseSetPublishedImagesFileEnv

// PublishedImage is a verified same-session Docker publication fact.
type PublishedImage = runtimeproto.ReleaseSetPublishedImage

type publishedImagesContextKey struct{}

// WithPublishedImages adds an owned copy of the framework-verified facts to a
// provider call. The Client validates and materializes them; callers cannot
// select the path or add this transport through process environment.
func WithPublishedImages(ctx context.Context, images []PublishedImage) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, publishedImagesContextKey{}, append([]PublishedImage(nil), images...))
}

// publishedImagesFromContext returns an owned copy for alternate provider
// adapters and tests. The bool distinguishes an explicit empty proof from no
// same-session transport.
func publishedImagesFromContext(ctx context.Context) ([]PublishedImage, bool) {
	if ctx == nil {
		return nil, false
	}
	images, ok := ctx.Value(publishedImagesContextKey{}).([]PublishedImage)
	return append([]PublishedImage(nil), images...), ok
}

func marshalPublishedImages(images []PublishedImage) ([]byte, error) {
	return runtimeproto.MarshalReleaseSetPublishedImages(images)
}
