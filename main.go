package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/types/infohash"
	"github.com/negrel/assert"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/samber/lo"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.uber.org/automaxprocs/maxprocs"

	"tyr/internal/client"
	"tyr/internal/config"
	"tyr/internal/meta"
	"tyr/internal/pkg/empty"
	"tyr/internal/pkg/random"
	_ "tyr/internal/platform" // deny compile on unsupported platform
	"tyr/internal/web"
)

func init() {
	if runtime.GOOS == "linux" {
		_, _ = maxprocs.Set()
	}
}

func main() {
	pflag.String("session-path", "", "client session path (default ~/.ve/)")
	pflag.String("config-file", "", "path to config file (default {session-path}/config.toml)")
	pflag.String("web", "127.0.0.1:8003", "web interface address")
	pflag.String("web-secret-token", "", "web interface address secret token")
	pflag.Uint16("p2p-port", 50047, "p2p listen port")

	pflag.Bool("log-json", false, "log as json format")
	pflag.String("log-level", "error", "log level")

	pflag.Bool("debug", false, "enable debug mode")

	// this avoids 'pflag: help requested' error when calling for help message.
	if slices.Contains(os.Args[1:], "--help") || slices.Contains(os.Args[1:], "-h") {
		pflag.Usage()
		fmt.Println("\nNote: extra options will override config file, but won't change config file.")
		return
	}

	pflag.Parse()

	viper.SetEnvPrefix("TYR")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	lo.Must0(viper.BindPFlags(pflag.CommandLine), "failed to parse combine argument with env")

	debug := viper.GetBool("debug")
	if debug {
		runtime.SetBlockProfileRate(10000)
		_, _ = fmt.Fprintln(os.Stderr, "enable debug mode")
	}

	jsonLog := viper.GetBool("log-json")

	if !jsonLog {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	logLevel := parseLogLevel(viper.GetString("log-level"))
	log.Logger = log.Logger.Level(logLevel)

	sessionPath := viper.GetString("session-path")

	if sessionPath == "" {
		sessionPath = defaultSessionPath()
	}

	createSessionDirectory(sessionPath)

	configFilePath := viper.GetString("config-file")

	if configFilePath == "" {
		configFilePath = filepath.Join(sessionPath, "config.toml")
	}

	if err := os.MkdirAll(sessionPath, os.ModePerm); err != nil {
		errExit("failed to create session path, must make sure you have permission", err)
	}

	cfg, err := config.LoadFromFile(configFilePath)
	if err != nil {
		errExit("failed to load config", err)
	}

	cfg.App.P2PPort = viper.GetUint16("p2p-port")

	address := viper.GetString("web")
	webToken := viper.GetString("web-secret-token")

	if webToken == "" {
		webToken = random.Base64Str(32)
		_, _ = fmt.Fprintln(os.Stderr, "no web secret token, generating new token:", strconv.Quote(webToken))
	}

	app := client.New(cfg, sessionPath)

	if e := app.Start(); e != nil {
		errExit("failed to listen on p2p port", e)
	}

	m := lo.Must(metainfo.LoadFromFile(`C:\Users\Trim21\Downloads\ubuntu-24.04-desktop-amd64.iso.torrent.patched`))
	assert.Equal(
		infohash.FromHexString("2aa4f5a7e209e54b32803d43670971c4c8caaa05"),
		m.HashInfoBytes(),
	)
	lo.Must0(app.AddTorrent(m, lo.Must(meta.FromTorrent(*m)), "C:\\Users\\Trim21\\Downloads\\ubuntu", strings.Split("a q e", " ")))

	var done = make(chan empty.Empty)

	go func() {
		server := web.New(app, webToken, debug)
		fmt.Println("start", address)
		err = http.ListenAndServe(address, server)
		done <- empty.Empty{}
		if err != nil {
			panic(err)
		}
	}()

	signalChan := make(chan os.Signal, 1)

	signal.Notify(
		signalChan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM,
	)

	go func() {
		<-signalChan
		done <- empty.Empty{}
	}()

	<-done
	fmt.Println("shutting down...")
	app.Shutdown()
}

func parseLogLevel(s string) zerolog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return zerolog.TraceLevel
	case "debug":
		return zerolog.DebugLevel
	case "info":
		return zerolog.InfoLevel
	case "warn":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	}

	errExit(fmt.Sprintf("unknown log level %q, only trace/debug/info/warn/error is allowed\n", s))

	return zerolog.NoLevel
}

func defaultSessionPath() string {
	h, err := os.UserHomeDir()
	if err != nil {
		errExit("failed to get home directory, please set session path with --session-path manually", err)
	}

	return filepath.Join(h, ".tyr")
}

func errExit(msg ...any) {
	_, _ = fmt.Fprint(os.Stderr, fmt.Sprintln(msg...))
	os.Exit(1)
}

func createSessionDirectory(sessionPath string) {
	err := os.MkdirAll(filepath.Join(sessionPath, "torrents"), os.ModePerm)
	if err != nil {
		errExit("fail to create directory for session", err)
	}
}
