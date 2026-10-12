// Package containerkey names a container destination the way the proven
// model's recordings and matchers do (design §3.1): by the container, not by
// the address the app happened to reach it at.
//
// A dependency the tests reach through a container's published port, or at a
// container's own IP, gets an address that changes from run to run:
// Testcontainers publishes on a random host port, and Docker hands out
// container IPs as it pleases. Its key does not change:
//
//	container:<image>:<containerPort>#<n>
//
// <image> is the image reference the container was created from, in Docker's
// short form with its tag (NormalizeImage); <containerPort> is the container's
// own port; n numbers that image's containers by their first connection
// within the scope, counting from 1 (Keyer). The agent resolves the key once
// per connection, when it accepts it, through the Docker API (Resolver).
//
// The key stands in for the address wherever a matcher compares one: in the
// recording (the mock's metadata, MetadataKey), in the learn step's operation
// key, and in place of a compared Host or :authority. A recording made
// without the proven switch, or of a destination that is not a container, has
// no key, and is compared by its address as before.
package containerkey

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"go.keploy.io/server/v3/pkg/models"
)

// MetadataKey is the mock metadata field that holds a container destination's
// key. Recordings made before container keys, and mocks of any other
// destination, do not have it.
const MetadataKey = "containerKey"

// prefix starts every key, and every `container:` entry of mock.real.
const prefix = "container:"

// Container is a container destination as the agent resolved it.
type Container struct {
	// ID is the container's Docker ID. It names the container only for the
	// run that saw it, so it is never written into a recording.
	ID string
	// Image is the image reference the container was created from, in the
	// form NormalizeImage gives.
	Image string
	// Port is the container's own port the connection reached.
	Port uint16
}

// Key is a parsed container key.
type Key struct {
	Image string
	Port  uint16
	// N numbers the image's containers within a scope, from 1.
	N int
}

// String is the key as recordings and matchers carry it.
func (k Key) String() string {
	return prefix + k.Image + ":" + strconv.Itoa(int(k.Port)) + "#" + strconv.Itoa(k.N)
}

// Repository is the key's image without its tag or digest, which is what a
// mock.real `container:<repo>` entry names.
func (k Key) Repository() string { return Repository(k.Image) }

// ErrNotKey is what Parse returns for a string that is not a container key.
var ErrNotKey = errors.New("not a container key")

// Parse reads a key String wrote. An image reference has no '#', and its
// last ':' before the '#' is the one String put before the port, so a tag
// (postgres:16), a registry port (localhost:5000/app) or a digest never makes
// the parse ambiguous.
func Parse(s string) (Key, error) {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return Key{}, fmt.Errorf("%w: %q has no %q prefix", ErrNotKey, s, prefix)
	}
	hash := strings.LastIndexByte(rest, '#')
	if hash < 0 {
		return Key{}, fmt.Errorf("%w: %q has no #<n>", ErrNotKey, s)
	}
	n, err := strconv.Atoi(rest[hash+1:])
	if err != nil || n < 1 || strconv.Itoa(n) != rest[hash+1:] {
		return Key{}, fmt.Errorf("%w: %q has no number from 1 after its #", ErrNotKey, s)
	}
	colon := strings.LastIndexByte(rest[:hash], ':')
	if colon <= 0 {
		return Key{}, fmt.Errorf("%w: %q has no image and port", ErrNotKey, s)
	}
	port, err := strconv.ParseUint(rest[colon+1:hash], 10, 16)
	if err != nil || port == 0 || strconv.FormatUint(port, 10) != rest[colon+1:hash] {
		return Key{}, fmt.Errorf("%w: %q has no port", ErrNotKey, s)
	}
	return Key{Image: rest[:colon], Port: uint16(port), N: n}, nil
}

// NormalizeImage writes an image reference the way Docker's short form does,
// so that two spellings of one reference give one key: postgres,
// postgres:latest, library/postgres:latest and docker.io/library/postgres:latest
// are all postgres:latest. A tag or digest is kept: a container of another
// version of an image is another dependency. A reference Docker's grammar does
// not read (a container created from an image ID) is kept as it is.
func NormalizeImage(ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ref
	}
	return reference.FamiliarString(reference.TagNameOnly(named))
}

// Repository is an image reference without its tag or digest, in Docker's
// short form: what a mock.real `container:<repo>` entry names (design §4).
func Repository(image string) string {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return image
	}
	return reference.FamiliarName(named)
}

// Recorded is the container key a recorded mock was stamped with, if any.
func Recorded(m *models.Mock) (string, bool) {
	if m == nil {
		return "", false
	}
	k := m.Spec.Metadata[MetadataKey]
	return k, k != ""
}

type ctxKey struct{}

// WithKey is ctx carrying a live connection's container key, which the proxy
// hands its parsers so a matcher can compare it with a mock's (Recorded).
func WithKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, key)
}

// FromContext is the container key of the connection a parser serves, if its
// destination is a container and the proven switch is on.
func FromContext(ctx context.Context) (string, bool) {
	k, _ := ctx.Value(ctxKey{}).(string)
	return k, k != ""
}
