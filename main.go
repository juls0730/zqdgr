package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
)

//go:embed embed/zqdgr.config.json
var zqdgrConfig []byte

type Config struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      string `json:"author"`
	License     string `json:"license"`
	Homepage    string `json:"homepage"`
	Repository  struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"repository"`
	Scripts map[string]string `json:"scripts"`
	Pattern string            `json:"pattern"`
	// Deprecated: use excludedGlobs instead
	ExcludedDirs    []string `json:"excluded_dirs"`
	ExcludedGlobs   []string `json:"excluded_files"`
	ShutdownSignal  string   `json:"shutdown_signal"`
	ShutdownTimeout int      `json:"shutdown_timeout"`
}

type Script struct {
	zqdgr        *ZQDGR
	command      *exec.Cmd
	mutex        sync.Mutex
	scriptName   string
	isRestarting bool
	wg           sync.WaitGroup
	// the exit code of the script, only set after the script has exited
	exitCode int
}

type ZQDGR struct {
	Config           Config
	WorkingDirectory string
	EnableWebSocket  bool
	WSServer         *WSServer
}

type WSServer struct {
	upgrader   websocket.Upgrader
	clients    map[*websocket.Conn]bool
	clientsMux sync.Mutex
}

func NewZQDGR(enableWebSocket bool, configDir string) *ZQDGR {
	zqdgr := &ZQDGR{
		WorkingDirectory: configDir,
	}

	err := zqdgr.loadConfig()
	if err != nil {
		log.Fatal(err)
		return nil
	}

	zqdgr.EnableWebSocket = enableWebSocket
	zqdgr.WSServer = &WSServer{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		clients:    make(map[*websocket.Conn]bool),
		clientsMux: sync.Mutex{},
	}

	return zqdgr
}

func (zqdgr *ZQDGR) NewCommand(scriptName string, args ...string) *exec.Cmd {
	if script, ok := zqdgr.Config.Scripts[scriptName]; ok {
		fullCmd := strings.Join(append([]string{script}, args...), " ")

		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/C", fullCmd)
		} else {
			cmd = exec.Command("sh", "-c", fullCmd)
		}

		cmd.Dir = zqdgr.WorkingDirectory

		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setpgid: true,
		}

		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin

		return cmd
	} else {
		return nil
	}
}

func (zqdgr *ZQDGR) NewScript(scriptName string, args ...string) *Script {
	command := zqdgr.NewCommand(scriptName, args...)

	if command == nil {
		log.Fatal("script not found")
		return nil
	}

	return &Script{
		zqdgr:        zqdgr,
		command:      command,
		scriptName:   scriptName,
		isRestarting: false,
	}
}

func (s *Script) Start() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.wg.Add(1)

	err := s.command.Start()
	if err != nil {
		s.wg.Done()
		return err
	}

	go func() {
		err := s.command.Wait()
		if err != nil {
			if exitError, ok := err.(*exec.ExitError); ok {
				s.exitCode = exitError.ExitCode()
			} else {
				// Other errors (e.g., process not found, permission denied)
				if !s.isRestarting {
					log.Printf("Error waiting for script %s: %v", s.scriptName, err)
					s.exitCode = 1
				}
			}
		}

		if !s.isRestarting {
			s.wg.Done()
		}
	}()

	return err
}

func (s *Script) Stop(lock bool) error {
	if lock {
		s.mutex.Lock()
		defer s.mutex.Unlock()
	}

	s.isRestarting = true

	if s.command.Process != nil {
		var signal syscall.Signal
		switch s.zqdgr.Config.ShutdownSignal {
		case "SIGINT":
			signal = syscall.SIGINT
		case "SIGTERM":
			signal = syscall.SIGTERM
		case "SIGQUIT":
			signal = syscall.SIGQUIT
		default:
			signal = syscall.SIGKILL
		}

		// make sure the process is not dead
		if s.command.ProcessState != nil && s.command.ProcessState.Exited() {
			return nil
		}

		if err := syscall.Kill(-s.command.Process.Pid, signal); err != nil {
			log.Printf("error killing previous process: %v", err)
			return err
		}
	}

	dead := make(chan bool)
	go func() {
		s.command.Wait()
		dead <- true
	}()

	shutdownTimeout := s.zqdgr.Config.ShutdownTimeout
	if shutdownTimeout == 0 {
		shutdownTimeout = 1
	}

	select {
	case <-dead:
	case <-time.After(time.Duration(shutdownTimeout) * time.Second):
		log.Println("Script failed to stop after kill signal, force killing")

		if err := syscall.Kill(-s.command.Process.Pid, syscall.SIGKILL); err != nil {
			log.Printf("error killing previous process: %v", err)
			return err
		}
	}

	return nil
}

func (s *Script) Restart() error {
	s.mutex.Lock()

	err := s.Stop(false)
	if err != nil {
		s.mutex.Unlock()
		return err
	}

	s.command = s.zqdgr.NewCommand(s.scriptName)

	if s.command == nil {
		// this should never happen
		log.Fatal("script not found")
		return nil
	}

	s.isRestarting = false

	s.mutex.Unlock()

	err = s.Start()

	// tell the websocket clients to refresh
	if s.zqdgr.EnableWebSocket {
		s.zqdgr.WSServer.clientsMux.Lock()
		for client := range s.zqdgr.WSServer.clients {
			err := client.WriteMessage(websocket.TextMessage, []byte("refresh"))
			if err != nil {
				log.Printf("error broadcasting refresh: %v", err)
				client.Close()
				delete(s.zqdgr.WSServer.clients, client)
			}
		}
		s.zqdgr.WSServer.clientsMux.Unlock()
	}

	return err
}

func (s *Script) Wait() {
	s.wg.Wait()
}

func (wsServer *WSServer) handleWs(w http.ResponseWriter, r *http.Request) {
	conn, err := wsServer.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("error upgrading connection: %v", err)
		return
	}

	wsServer.clientsMux.Lock()
	wsServer.clients[conn] = true
	wsServer.clientsMux.Unlock()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			wsServer.clientsMux.Lock()
			delete(wsServer.clients, conn)
			wsServer.clientsMux.Unlock()
			break
		}
	}
}

func (zqdgr *ZQDGR) loadConfig() error {
	data, err := os.ReadFile(path.Join(zqdgr.WorkingDirectory, "zqdgr.config.json"))
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&zqdgr.Config)
		if err != nil {
			return fmt.Errorf("error parsing config file: %v", err)
		}
	} else {
		zqdgr.Config = Config{
			Scripts: map[string]string{
				"build": "go build",
				"run":   "go run main.go",
			},
			Pattern: "**/*.go",
		}
	}

	if zqdgr.Config.ExcludedDirs != nil {
		fmt.Printf("WARNING: the 'excluded_dirs' key is deprecated, please use 'excluded_globs' instead\n")

		zqdgr.Config.ExcludedGlobs = append(zqdgr.Config.ExcludedGlobs, zqdgr.Config.ExcludedDirs...)
	}

	return nil
}

func main() {
	noWs := flag.Bool("no-ws", false, "Disable WebSocket server")
	configDir := flag.String("config", ".", "Path to the config directory")
	disableReloadConfig := flag.Bool("no-reload-config", false, "Do not restart ZQDGR on config file change")
	flag.StringVar(configDir, "C", *configDir, "Path to the config directory")

	flag.Parse()

	originalArgs := os.Args
	os.Args = flag.Args()

	zqdgr := NewZQDGR(*noWs, *configDir)
	if zqdgr == nil {
		return
	}

	var command string
	var commandArgs []string

	for i, arg := range os.Args {
		if arg == "--" {
			if i+2 < len(os.Args) {
				commandArgs = os.Args[i+2:]
			}
			break
		}

		if command == "" {
			command = arg
			continue
		}

		commandArgs = append(commandArgs, arg)
	}

	watchMode := false
	var scriptName string
	switch command {
	case "init":
		config, err := os.Create("zqdgr.config.json")
		if err != nil {
			log.Fatal(err)
		}

		_, err = config.Write(zqdgrConfig)
		if err != nil {
			log.Fatal(err)
		}

		fmt.Println("zqdgr.config.json created successfully")
		return
	case "new":
		var projectName string
		var gitRepo string
		// if no project name was provided, or if the first argument has a slash (it's a url)
		if len(commandArgs) < 1 || strings.Contains(commandArgs[0], "/") {
			fmt.Printf("What is the name of the project: ")
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Scan()
			projectName = scanner.Text()

			if len(commandArgs) > 0 {
				gitRepo = commandArgs[0]
			}
		} else {
			projectName = commandArgs[0]

			if len(commandArgs) > 1 {
				gitRepo = commandArgs[1]
			}
		}

		if _, err := os.Stat(projectName); err == nil {
			log.Fatal("project already exists")
		}

		err := os.MkdirAll(projectName, 0755)
		if err != nil {
			log.Fatal(err)
		}

		cwd, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		projectDir := filepath.Join(cwd, projectName)

		fmt.Println("Initializing git repository")
		cmd := exec.Command("git", "init")
		cmd.Dir = projectDir

		err = cmd.Run()
		if err != nil {
			log.Fatal(err)
		}

		fmt.Println("Initializing zqdgr project")

		zqdgrExe, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}

		cmd = exec.Command(zqdgrExe, "init")
		cmd.Dir = projectDir

		err = cmd.Run()
		if err != nil {
			log.Fatal(err)
		}

		fmt.Printf("What is the module path for %s (e.g. github.com/user/project): ", projectName)
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		if err != nil {
			log.Fatal(err)
		}

		projectPath := scanner.Text()
		cmd = exec.Command("go", "mod", "init", projectPath)
		cmd.Dir = projectDir

		err = cmd.Run()

		if err != nil {
			log.Fatal(err)
		}

		goMain, err := os.Create(filepath.Join(projectDir, "main.go"))
		if err != nil {
			log.Fatal(err)
		}

		goMain.WriteString("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"Hello, World!\")\n}\n")

		if gitRepo != "" {
			// execute a create script
			tempDir, err := os.MkdirTemp(projectName, "zqdgr")
			if err != nil {
				log.Fatal(err)
			}
			defer os.RemoveAll(tempDir)

			fmt.Printf("Cloning %s\n", gitRepo)
			cmd = exec.Command("git", "clone", gitRepo, tempDir)

			err = cmd.Run()
			if err != nil {
				log.Fatal(err)
			}

			cmd = exec.Command("zqdgr", "build")
			cmd.Dir = tempDir

			err = cmd.Run()
			if err != nil {
				log.Fatal(err)
			}

			cmd = exec.Command(filepath.Join(projectDir, filepath.Base(tempDir), "main"), projectDir)
			cmd.Dir = projectDir
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			err = cmd.Run()
			if err != nil {
				log.Fatal(err)
			}
		}

		return
	case "watch":
		if len(commandArgs) < 1 {
			log.Fatal("please specify a script to run")
		}
		watchMode = true
		for i := range commandArgs {
			if strings.HasPrefix(commandArgs[i], "-") {
				continue
			}

			scriptName = commandArgs[i]
		}
	default:
		scriptName = command
	}

	script := zqdgr.NewScript(scriptName, commandArgs...)

	if err := script.Start(); err != nil {
		log.Fatal(err)
	}

	go func() {
		processSignalChannel := make(chan os.Signal, 1)
		signal.Notify(processSignalChannel, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		<-processSignalChannel

		log.Println("Received signal, exiting...")
		if script.command != nil {
			var signal syscall.Signal
			switch zqdgr.Config.ShutdownSignal {
			case "SIGINT":
				signal = syscall.SIGINT
			case "SIGTERM":
				signal = syscall.SIGTERM
			case "SIGQUIT":
				signal = syscall.SIGQUIT
			default:
				signal = syscall.SIGKILL
			}

			syscall.Kill(-script.command.Process.Pid, signal)
		}

		os.Exit(0)
	}()

	if watchMode {
		if !*noWs {
			zqdgr.EnableWebSocket = true

			go func() {
				http.HandleFunc("/ws", zqdgr.WSServer.handleWs)
				log.Printf("WebSocket server running on :2067")
				if err := http.ListenAndServe(":2067", nil); err != nil {
					log.Printf("WebSocket server error: %v", err)
				}
			}()
		}

		if zqdgr.Config.Pattern == "" {
			log.Fatal("watch pattern not specified in config")
		}

		// make sure the pattern is valid
		var paternArray []string
		var currentPattern string
		inMatch := false
		// iterate over every letter in the pattern
		for _, p := range zqdgr.Config.Pattern {
			if string(p) == "{" {
				if inMatch {
					log.Fatal("unmatched { in pattern")
				}

				inMatch = true
			}

			if string(p) == "}" {
				if !inMatch {
					log.Fatal("unmatched } in pattern")
				}

				inMatch = false
			}

			if string(p) == "," && !inMatch {
				paternArray = append(paternArray, currentPattern)
				currentPattern = ""
				inMatch = false
				continue
			}

			currentPattern += string(p)
		}

		if inMatch {
			log.Fatal("unmatched } in pattern")
		}

		if currentPattern != "" {
			paternArray = append(paternArray, currentPattern)
		}

		watcherConfig := WatcherConfig{
			excludedGlobs: globList(zqdgr.Config.ExcludedGlobs),
			pattern:       paternArray,
		}

		watcher, err := NewWatcher(&watcherConfig)
		if err != nil {
			log.Fatal(err)
		}

		defer watcher.Close()

		err = watcher.AddFiles()
		if err != nil {
			log.Fatal(err)
		}

		err = watcher.AddFile(path.Join(zqdgr.WorkingDirectory, "zqdgr.config.json"))
		if err != nil {
			log.Fatal(err)
		}

		// We use this timer to deduplicate events.
		var (
			// Wait 100ms for new events; each new event resets the timer.
			waitFor = 100 * time.Millisecond

			// Keep track of the timers, as path → timer.
			mu     sync.Mutex
			timers = make(map[string]*time.Timer)
		)
		go func() {
			for {
				select {
				case event, ok := <-watcher.(NotifyWatcher).watcher.Events:
					if !ok {
						return
					}

					mu.Lock()
					timer, ok := timers[event.Name]
					mu.Unlock()

					if !ok {
						timer = time.AfterFunc(waitFor, func() {
							if event.Op&fsnotify.Remove == fsnotify.Remove || event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
								if os.Getenv("ZQDGR_DEBUG") != "" {
									fmt.Println("File changed:", event.Name)
								}

								if strings.HasSuffix(event.Name, "zqdgr.config.json") {
									// re-exec the exact same command
									if !*disableReloadConfig {
										log.Println("zqdgr.config.json has changed, restarting...")
										executable, err := os.Executable()
										if err != nil {
											log.Fatal(err)
										}

										err = script.Stop(true)
										if err != nil {
											log.Fatal(err)
										}

										err = syscall.Exec(executable, originalArgs, os.Environ())
										if err != nil {
											log.Fatal(err)
										}

										panic("unreachable")
									}
								}

								if directoryShouldBeTracked(&watcherConfig, event.Name) {
									watcher.(NotifyWatcher).watcher.Add(event.Name)
								}

								if pathMatches(&watcherConfig, event.Name) {
									script.Restart()
								}

							}
						})
						timer.Stop()

						mu.Lock()
						timers[event.Name] = timer
						mu.Unlock()
					}

					timer.Reset(waitFor)
				case err := <-watcher.(NotifyWatcher).watcher.Errors:
					if err == nil {
						continue
					}

					if v, ok := err.(*os.SyscallError); ok {
						if v.Err == syscall.EINTR {
							continue
						}
						log.Fatal("watcher.Error: SyscallError:", v)
					}
					log.Fatal("watcher.Error:", err)

				}
			}
		}()
	}

	script.Wait()
	os.Exit(script.exitCode)
}
