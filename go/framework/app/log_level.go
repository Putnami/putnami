package app

import (
	"context"
	"time"

	"go.putnami.dev/config"
	"go.putnami.dev/logger"
)

// loggerOptions is the logger config block. Level is the minimum level
// (logger.level); RefreshSeconds is how often a started application re-reads
// it, and 0 turns the re-read off.
type loggerOptions struct {
	Level          string `json:"level"`
	RefreshSeconds int    `json:"refreshSeconds" default:"30"`
}

var loggerSchema = config.Config[loggerOptions]("logger")

// defaultRefreshSeconds is the re-read period when the logger block cannot be
// read at start; it matches the refreshSeconds default.
const defaultRefreshSeconds = 30

// replaceLevelWatchWait bounds how long a new watcher waits for the one it
// replaces. A canceled watcher applies no level once its read returns, so a
// hung config read never blocks the replacement past this wait.
var replaceLevelWatchWait = 5 * time.Second

// applyLogLevel sets the live log level from logger.level, read from every
// config source, and returns the block. A block that cannot be read keeps the
// current level, and the watcher re-reads it every defaultRefreshSeconds.
func (a *Application) applyLogLevel(ctx context.Context) loggerOptions {
	opts, err := config.LoadContext(ctx, loggerSchema)
	if err != nil {
		a.log.Warn("the logger config block is unreadable: the log level stays", logger.ErrorAttr(err))
		return loggerOptions{RefreshSeconds: defaultRefreshSeconds}
	}
	logger.SetLevel(logger.ResolveLevel(opts.Level))
	return opts
}

// watchLogLevel re-reads logger.level every opts.RefreshSeconds and applies a
// changed value, until Stop. It replaces a watcher a previous Start left.
func (a *Application) watchLogLevel(opts loggerOptions) {
	if opts.RefreshSeconds <= 0 {
		return
	}
	a.startLevelWatch(time.Duration(opts.RefreshSeconds)*time.Second, readLocalLogLevel)
}

// startLevelWatch runs logger.WatchLevel with read every interval until
// stopWatchingLogLevel, replacing a previous watcher.
func (a *Application) startLevelWatch(interval time.Duration, read func(context.Context) (string, error)) {
	wait, cancelWait := context.WithTimeout(context.Background(), replaceLevelWatchWait)
	a.stopWatchingLogLevel(wait)
	cancelWait()
	ctx, cancel := context.WithCancel(context.Background())
	done := logger.WatchLevel(ctx, interval, read)
	a.mu.Lock()
	a.stopLevelWatch = func(wait context.Context) {
		cancel()
		select {
		case <-done:
		case <-wait.Done():
		}
	}
	a.mu.Unlock()
}

// stopWatchingLogLevel ends the watcher watchLogLevel started, if any, and
// returns once it applies no further level, or when ctx ends first: a config
// read in flight never holds Stop past its deadline.
func (a *Application) stopWatchingLogLevel(ctx context.Context) {
	a.mu.Lock()
	stop := a.stopLevelWatch
	a.stopLevelWatch = nil
	a.mu.Unlock()
	if stop != nil {
		stop(ctx)
	}
}

// readLocalLogLevel reads logger.level from the config sources that load
// without the network: the conf files and CONFIG_DATA. A source that
// implements config.ContextSource may block on a remote call, so the watcher
// skips it.
func readLocalLogLevel(ctx context.Context) (string, error) {
	var local []config.Source
	for _, source := range config.DiscoverSources() {
		if _, remote := source.(config.ContextSource); !remote {
			local = append(local, source)
		}
	}
	if len(local) == 0 {
		// LoadContext with no source discovers every source, remote ones too.
		return "", nil
	}
	opts, err := config.LoadContext(ctx, loggerSchema, local...)
	return opts.Level, err
}
