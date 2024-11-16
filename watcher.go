// There is some dead code in here because I pulled out the parts I needed to get it working, deal with this later lol.

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"

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
	Watch(jobs chan<- string)
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

func (n NotifyWatcher) Watch(jobs chan<- string) {
	for {
		select {
		case ev := <-n.watcher.Events:
			if ev.Op&fsnotify.Remove == fsnotify.Remove || ev.Op&fsnotify.Write == fsnotify.Write || ev.Op&fsnotify.Create == fsnotify.Create {
				// Assume it is a directory and track it.
				if directoryShouldBeTracked(n.cfg, ev.Name) {
					n.watcher.Add(ev.Name)
				}
				if pathMatches(n.cfg, ev.Name) {
					jobs <- ev.Name
				}
			}

		case err := <-n.watcher.Errors:
			if v, ok := err.(*os.SyscallError); ok {
				if v.Err == syscall.EINTR {
					continue
				}
				log.Fatal("watcher.Error: SyscallError:", v)
			}
			log.Fatal("watcher.Error:", err)
		}
	}
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
