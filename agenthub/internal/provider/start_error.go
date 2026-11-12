package provider

import "errors"

// ErrNativeSessionMissing means that a provider cannot restore its native
// conversation. Starting a fresh conversation would silently lose context.
var ErrNativeSessionMissing = errors.New("native provider session is missing")
