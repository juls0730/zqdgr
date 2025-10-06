package main

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar"
	"github.com/fsnotify/fsnotify"
)

type globList []string

func (g *globList) Matches(value string) bool {
	for _, v := range *g {
		// if the pattern matches a filepath pattern
		if match, err := filepath.Match(v, value); err != nil {
			log.Fatalf("Bad pattern \"%s\": %s", v, err.Error())
		} else if match {
			return true
		}

		// or if the path starts with the pattern
		if strings.HasSuffix(value, v) {
			return true
		}
	}

	return false
}

func matchesPattern(pattern []string, path string) bool {
	for _, p := range pattern {
		slog.Debug("checking path against pattern", "pattern", p, "path", path)
		if matched, _ := doublestar.Match(p, path); matched {
			slog.Debug("path matches pattern", "pattern", p, "path", path)
			return true
		}
	}

	return false
}

func pathShouldBeTracked(cfg *WatcherConfig, path string) bool {
	base := filepath.Dir(path)

	slog.Debug("checking file against path", "path", path, "base", base)

	if cfg.excludedGlobs.Matches(base) {
		slog.Debug("file is excluded", "path", path)
		return false
	}

	return matchesPattern(cfg.pattern, path)
}

func pathMatches(cfg *WatcherConfig, path string) bool {
	return matchesPattern(cfg.pattern, path)
}

type WatcherConfig struct {
	excludedGlobs globList
	pattern       globList
}

type FileWatcher interface {
	Close() error
	AddFiles() error
	AddFile(path string) error
	add(path string) error
	getConfig() *WatcherConfig
}

type NotifyWatcher struct {
	watcher *fsnotify.Watcher
	cfg     *WatcherConfig
}

func (n NotifyWatcher) Close() error {
	return n.watcher.Close()
}

func (n NotifyWatcher) AddFile(path string) error {
	slog.Debug("manually adding file", "file", path)

	return n.add(path)
}

func (n NotifyWatcher) AddFiles() error {
	return addFiles(n)
}

func (n NotifyWatcher) add(path string) error {
	return n.watcher.Add(path)
}

func (n NotifyWatcher) getConfig() *WatcherConfig {
	return n.cfg
}

func NewWatcher(cfg *WatcherConfig) (FileWatcher, error) {
	if cfg == nil {
		err := errors.New("no config specified")
		return nil, err
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return NotifyWatcher{
		watcher: w,
		cfg:     cfg,
	}, nil
}

func addFiles(fw FileWatcher) error {
	cfg := fw.getConfig()
	for _, pattern := range cfg.pattern {
		slog.Debug("adding pattern", "pattern", pattern)

		matches, err := doublestar.Glob(pattern)
		if err != nil {
			log.Fatalf("Bad pattern \"%s\": %s", pattern, err.Error())
		}

		trackedDirs := make(map[string]bool)
		for _, match := range matches {
			base := filepath.Dir(match)
			// this allows us to track file creations and deletions
			if !trackedDirs[base] {
				if cfg.excludedGlobs.Matches(base) {
					slog.Debug("directory is excluded", "file", match)
					continue
				}

				trackedDirs[base] = true

				slog.Debug("adding directory", "dir", base)

				if err := fw.add(base); err != nil {
					return fmt.Errorf("FileWatcher.Add(): %v", err)
				}
			}

			slog.Debug("adding file", "file", match)

			if pathShouldBeTracked(cfg, match) {
				slog.Debug("path should be tracked", "file", match)

				if err := fw.add(match); err != nil {
					return fmt.Errorf("FileWatcher.Add(): %v", err)
				}
			}
		}
	}

	return nil
}
