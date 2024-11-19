package main

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"github.com/bmatcuk/doublestar"
	"github.com/fsnotify/fsnotify"
)

type globList []string

func (g *globList) Matches(value string) bool {
	for _, v := range *g {
		if match, err := filepath.Match(v, value); err != nil {
			log.Fatalf("Bad pattern \"%s\": %s", v, err.Error())
		} else if match {
			return true
		}
	}
	return false
}

func matchesPattern(pattern []string, path string) bool {
	for _, p := range pattern {
		if matched, _ := doublestar.Match(p, path); matched {
			return true
		}
	}
	return false
}

func directoryShouldBeTracked(cfg *WatcherConfig, path string) bool {
	base := filepath.Dir(path)
	return matchesPattern(cfg.pattern, path) && !cfg.excludedDirs.Matches(base)
}

func pathMatches(cfg *WatcherConfig, path string) bool {
	return matchesPattern(cfg.pattern, path)
}

type WatcherConfig struct {
	excludedDirs globList
	pattern      globList
}

type FileWatcher interface {
	Close() error
	AddFiles() error
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
		matches, err := doublestar.Glob(pattern)
		if err != nil {
			log.Fatalf("Bad pattern \"%s\": %s", pattern, err.Error())
		}
		for _, match := range matches {
			if directoryShouldBeTracked(cfg, match) {
				if err := fw.add(match); err != nil {
					return fmt.Errorf("FileWatcher.Add(): %v", err)
				}
			}
		}

	}
	return nil
}
