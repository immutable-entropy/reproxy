package capture

import "context"

type sourceKey struct{}

type sourceVal struct {
	source Source
	origin string
}

// WithSource tags ctx with who is making a connection, for work done on the
// way to it that is recorded as a flow of its own — a DNS lookup made while
// dialling a replay is listed as the replay's (see package dns).
func WithSource(ctx context.Context, source Source, origin string) context.Context {
	return context.WithValue(ctx, sourceKey{}, sourceVal{source, origin})
}

// SourceFrom returns what WithSource put on ctx; ok is false when nothing
// did.
func SourceFrom(ctx context.Context) (source Source, origin string, ok bool) {
	v, ok := ctx.Value(sourceKey{}).(sourceVal)
	return v.source, v.origin, ok
}
