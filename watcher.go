package main

import (
	"errors"
	"fmt"
	"log"
	"os"
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
		if matched, _ := doublestar.Match(p, path); matched {
			return true
		}
	}

	return false
}

func directoryShouldBeTracked(cfg *WatcherConfig, path string) bool {
	base := filepath.Dir(path)

	if os.Getenv("ZQDGR_DEBUG") != "" {
		log.Printf("checking %s against %s %v\n", path, base, *cfg)
	}

	if cfg.excludedGlobs.Matches(base) {
		if os.Getenv("ZQDGR_DEBUG") != "" {
			log.Printf("%s is excluded\n", base)
		}
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
	if os.Getenv("ZQDGR_DEBUG") != "" {
		log.Printf("manually adding file\n")
	}

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
		if os.Getenv("ZQDGR_DEBUG") != "" {
			fmt.Printf("processing glob %s\n", pattern)
		}

		matches, err := doublestar.Glob(pattern)
		if err != nil {
			log.Fatalf("Bad pattern \"%s\": %s", pattern, err.Error())
		}

		for _, match := range matches {
			if os.Getenv("ZQDGR_DEBUG") != "" {
				log.Printf("checking %s\n", match)
			}

			if directoryShouldBeTracked(cfg, match) {
				if os.Getenv("ZQDGR_DEBUG") != "" {
					log.Printf("%s is not excluded\n", match)
				}

				if err := fw.add(match); err != nil {
					return fmt.Errorf("FileWatcher.Add(): %v", err)
				}
			}
		}
	}

	return nil
}
