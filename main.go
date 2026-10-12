// Command sloflixfs mounts the sloflix.com catalog as a read-only FUSE filesystem in Jellyfin's layout.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/Lenart12/sloflixfs/internal/fusefs"
	"github.com/Lenart12/sloflixfs/internal/sloflix"
)

func main() {
	cfg, mount, allowOther := parseFlags()
	if err := sloflix.Start(cfg); err != nil {
		log.Fatal(err)
	}
	if err := sloflix.Login(); err != nil {
		log.Fatal(err)
	}
	server, err := mountFS(mount, allowOther)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mounted on %s", mount)
	go sloflix.Crawl()
	go unmountOnSignal(server, mount)
	server.Wait()
}

func parseFlags() (cfg sloflix.Config, mount string, allowOther bool) {
	userCache, _ := os.UserCacheDir()
	flag.StringVar(&mount, "mount", "", "mountpoint (required)")
	flag.StringVar(&cfg.CacheDir, "cache", filepath.Join(userCache, "sloflixfs"), "cache directory")
	flag.DurationVar(&cfg.Refresh, "refresh", 12*time.Hour, "how long catalog listings are cached")
	flag.DurationVar(&cfg.Revalidate, "revalidate", 7*24*time.Hour, "how often each title's size and subtitle are revalidated in the background")
	flag.IntVar(&cfg.ProbeCache, "probe-cache", 500, "max new titles whose start is saved for Jellyfin's first probe, about 10-16 MB each until probed (0 = off)")
	flag.Float64Var(&cfg.Rate, "rate", 1, "max API requests per second")
	flag.IntVar(&cfg.Concurrency, "concurrency", 3, "max upstream lookups/fetches in flight (playback is exempt)")
	flag.IntVar(&cfg.LimitMovies, "limit-movies", 0, "list only the newest N movies (0 = all), for testing")
	flag.IntVar(&cfg.LimitShows, "limit-shows", 0, "list only the newest N shows (0 = all), for testing")
	flag.BoolVar(&allowOther, "allow-other", false, "let other users (e.g. Jellyfin in Docker) access the mount")
	flag.StringVar(&cfg.Username, "user", "", "sloflix username")
	flag.StringVar(&cfg.Password, "pass", "", "sloflix password; prefer the environment variable, since other users can see arguments")
	flag.StringVar(&cfg.JellyfinURL, "jellyfin-url", "", "Jellyfin's address (e.g. http://jellyfin:8096), to scan its sloflix libraries when new titles are ready (empty = off)")
	flag.StringVar(&cfg.JellyfinKey, "jellyfin-key", "", "Jellyfin API key (Dashboard > API Keys)")
	flag.StringVar(&cfg.JellyfinPath, "jellyfin-path", "/media/sloflix/library", "the mount's path as Jellyfin sees it; libraries under it are scanned")
	if err := envDefaults(flag.CommandLine, os.Getenv); err != nil {
		log.Fatal(err)
	}
	flag.Parse()
	if mount == "" || cfg.Username == "" || cfg.Password == "" {
		log.Fatal("usage: sloflixfs -mount DIR, with credentials in SLOFLIX_USER and SLOFLIX_PASS (or -user, -pass); -h lists all options")
	}
	if (cfg.JellyfinURL == "") != (cfg.JellyfinKey == "") {
		log.Fatal("-jellyfin-url and -jellyfin-key go together")
	}
	if cfg.Rate <= 0 || cfg.Concurrency <= 0 || cfg.Refresh <= 0 || cfg.Revalidate <= 0 {
		log.Fatal("-rate, -concurrency, -refresh and -revalidate must be positive")
	}
	return cfg, mount, allowOther
}

func mountFS(mount string, allowOther bool) (*fuse.Server, error) {
	// Clear a dead mount left by a previous crash, or mounting fails. Needs root (e.g. in Docker).
	if _, err := os.Stat(mount); errors.Is(err, syscall.ENOTCONN) {
		syscall.Unmount(mount, syscall.MNT_DETACH)
	}
	if err := os.MkdirAll(mount, 0755); err != nil {
		return nil, err
	}
	timeout := time.Hour
	return fs.Mount(mount, fusefs.NewRoot(), &fs.Options{
		EntryTimeout: &timeout,
		AttrTimeout:  &timeout,
		MountOptions: fuse.MountOptions{
			AllowOther:         allowOther,
			FsName:             "sloflix",
			Name:               "sloflixfs",
			SyncRead:           true,
			DisableReadDirPlus: true,
			MaxReadAhead:       1 << 20,
			Options:            []string{"ro"},
			DirectMount:        true,
		},
	})
}

func unmountOnSignal(server *fuse.Server, mount string) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	// A lazy unmount also reaches busy copies of the mount, such as Jellyfin's in Docker, so stopping with
	// files open leaves no dead mount. It needs root; otherwise fall back to fusermount.
	if err := syscall.Unmount(mount, syscall.MNT_DETACH); err != nil {
		if err := server.Unmount(); err != nil {
			log.Printf("unmount: %v", err)
		}
		return
	}
	// Open files keep a detached mount's connection alive, so don't wait for them.
	time.AfterFunc(2*time.Second, func() { os.Exit(0) })
}

// envDefaults lets every flag be set from the environment: -probe-cache as SLOFLIX_PROBE_CACHE. The command
// line wins.
func envDefaults(fset *flag.FlagSet, getenv func(string) string) error {
	var err error
	fset.VisitAll(func(f *flag.Flag) {
		name := "SLOFLIX_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		f.Usage += " (env " + name + ")"
		if v := getenv(name); v != "" && err == nil {
			if e := f.Value.Set(v); e != nil {
				err = fmt.Errorf("%s=%q: %v", name, v, e)
			}
		}
	})
	return err
}
