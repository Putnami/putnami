package events

import "time"

func normalizeHandlerOptions(opts HandlerOptions) HandlerOptions {
	defaults := DefaultHandlerOptions()
	if opts.Distribution == "" {
		opts.Distribution = defaults.Distribution
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = defaults.MaxRetries
	}
	if opts.BaseBackoff == 0 {
		opts.BaseBackoff = defaults.BaseBackoff
	}
	if opts.MaxBackoff == 0 {
		opts.MaxBackoff = defaults.MaxBackoff
	}
	if opts.Timeout == 0 {
		opts.Timeout = defaults.Timeout
	}
	if opts.Overflow == "" {
		opts.Overflow = defaults.Overflow
	}
	if opts.Ack == "" {
		opts.Ack = defaults.Ack
	}
	return opts
}

func handlerTimeout(opts HandlerOptions) time.Duration {
	opts = normalizeHandlerOptions(opts)
	return opts.Timeout
}
