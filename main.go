package main

import (
	"bufio"
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
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
)

var executableName string

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
	Scripts        map[string]string `json:"scripts"`
	Pattern        string            `json:"pattern"`
	ExcludedDirs   []string          `json:"excluded_dirs"`
	ShutdownSignal string            `json:"shutdown_signal"`
}

type Script struct {
	command      *exec.Cmd
	mutex        sync.Mutex
	scriptName   string
	isRestarting bool
	wg           sync.WaitGroup
	// the exit code of the script, only set after the script has exited
	exitCode int
}

func flattenZQDGRScript(commandString string) string {
	keys := make([]string, 0, len(config.Scripts))
	for k := range config.Scripts {
		keys = append(keys, k)
	}

	// Sort the keys in descending order in order to prevent scripts that might be substrings of other scripts to
	// evaluate first.
	sort.Slice(keys, func(i, j int) bool {
		return len(keys[i]) > len(keys[j])
	})

	// escape scripts to be evaluated via regex
	escapedKeys := make([]string, len(keys))
	for i, key := range keys {
		escapedKeys[i] = regexp.QuoteMeta(key)
	}
	pattern := `\b(` + executableName + `)\b` + `\s+` + `\b(` + strings.Join(escapedKeys, "|") + `)\b`

	re := regexp.MustCompile(pattern)

	currentCommand := commandString
	for {
		previousCommand := currentCommand
		currentCommand = re.ReplaceAllStringFunc(currentCommand, func(match string) string {
			// match the script name, not the whole `zqdgr script` command
			match = strings.Split(match, " ")[1]

			if val, ok := config.Scripts[match]; ok {
				return val
			}
			return match
		})

		// If the current command has not changed, we have completely evaluated the command.
		if currentCommand == previousCommand {
			break
		}
	}

	if re.MatchString(currentCommand) {
		fmt.Println("Error: circular dependency detected in scripts")
		os.Exit(1)
	}

	return currentCommand
}

func NewCommand(scriptName string, args ...string) *exec.Cmd {
	if script, ok := config.Scripts[scriptName]; ok {
		fullCmd := strings.Join(append([]string{script}, args...), " ")

		fullCmd = flattenZQDGRScript(fullCmd)

		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/C", fullCmd)
		} else {
			cmd = exec.Command("sh", "-c", fullCmd)
		}

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

func NewScript(scriptName string, args ...string) *Script {
	command := NewCommand(scriptName, args...)

	if command == nil {
		log.Fatal("script not found")
		return nil
	}

	return &Script{
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
				log.Printf("Error waiting for script %s: %v", s.scriptName, err)
				s.exitCode = 1
			}
		}

		if !s.isRestarting {
			s.wg.Done()
		}
	}()

	return err
}

func (s *Script) Restart() error {
	s.mutex.Lock()

	s.isRestarting = true

	if s.command.Process != nil {
		var signal syscall.Signal
		switch config.ShutdownSignal {
		case "SIGINT":
			signal = syscall.SIGINT
		case "SIGTERM":
			signal = syscall.SIGTERM
		case "SIGQUIT":
			signal = syscall.SIGQUIT
		default:
			signal = syscall.SIGKILL
		}

		if err := syscall.Kill(-s.command.Process.Pid, signal); err != nil {
			log.Printf("error killing previous process: %v", err)
		}
	}

	s.command = NewCommand(s.scriptName)

	if s.command == nil {
		// this should never happen
		log.Fatal("script not found")
		return nil
	}

	s.isRestarting = false

	s.mutex.Unlock()

	err := s.Start()

	// tell the websocket clients to refresh
	if enableWebSocket {
		clientsMux.Lock()
		for client := range clients {
			err := client.WriteMessage(websocket.TextMessage, []byte("refresh"))
			if err != nil {
				log.Printf("error broadcasting refresh: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
		clientsMux.Unlock()
	}

	return err
}

func (s *Script) Wait() {
	s.wg.Wait()
}

func handleWs(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("error upgrading connection: %v", err)
		return
	}

	clientsMux.Lock()
	clients[conn] = true
	clientsMux.Unlock()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			clientsMux.Lock()
			delete(clients, conn)
			clientsMux.Unlock()
			break
		}
	}
}

var (
	enableWebSocket = false
	config          Config
	script          *Script
	upgrader        = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	clients    = make(map[*websocket.Conn]bool)
	clientsMux sync.Mutex
)

func loadConfig() error {
	data, err := os.ReadFile("zqdgr.config.json")
	if err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("error parsing config file: %v", err)
		}
	} else {
		config = Config{
			Scripts: map[string]string{
				"build": "go build",
				"run":   "go run main.go",
			},
			Pattern: "**/*.go",
		}
	}

	return nil
}

func main() {
	noWs := flag.Bool("no-ws", false, "Disable WebSocket server")
	flag.Parse()

	if err := loadConfig(); err != nil {
		log.Fatal(err)
	}

	var command string
	var commandArgs []string

	// get the name of the executable, and if it's a path then get the base name
	// this is mainly for testing
	executableName = path.Base(os.Args[0])

	for i, arg := range os.Args[1:] {
		if arg == "--" {
			commandArgs = os.Args[i+2:]
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
		for i := 0; i < len(commandArgs); i++ {
			if strings.HasPrefix(commandArgs[i], "-") {
				continue
			}

			scriptName = commandArgs[i]
		}
	default:
		scriptName = command
	}

	script = NewScript(scriptName, commandArgs...)

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
			switch config.ShutdownSignal {
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
			enableWebSocket = true

			go func() {
				http.HandleFunc("/ws", handleWs)
				log.Printf("WebSocket server running on :2067")
				if err := http.ListenAndServe(":2067", nil); err != nil {
					log.Printf("WebSocket server error: %v", err)
				}
			}()
		}

		if config.Pattern == "" {
			log.Fatal("watch pattern not specified in config")
		}

		var paternArray []string
		var currentPattern string
		inMatch := false
		// iterate over every letter in the pattern
		for _, p := range config.Pattern {
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
			excludedDirs: globList(config.ExcludedDirs),
			pattern:      paternArray,
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
								fmt.Println("File changed:", event.Name)
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
