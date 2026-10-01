package releaseset

import (
	"context"

	runtimeproto "go.putnami.dev/protocol/runtime"
)

// PublishedImagesFileEnv is the private framework-to-reserved-provider file.
const PublishedImagesFileEnv = runtimeproto.ReleaseSetPublishedImagesFileEnv

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

// PublishedImagesFromContext returns an owned copy for alternate provider
// adapters and tests. The bool distinguishes an explicit empty proof from no
// same-session transport.
func PublishedImagesFromContext(ctx context.Context) ([]PublishedImage, bool) {
	if ctx == nil {
		return nil, false
	}
	images, ok := ctx.Value(publishedImagesContextKey{}).([]PublishedImage)
	return append([]PublishedImage(nil), images...), ok
}

// ParsePublishedImages delegates to the protocol/runtime authority.
func ParsePublishedImages(data []byte) ([]PublishedImage, error) {
	return runtimeproto.ParseReleaseSetPublishedImages(data)
}

func marshalPublishedImages(images []PublishedImage) ([]byte, error) {
	return runtimeproto.MarshalReleaseSetPublishedImages(images)
}
