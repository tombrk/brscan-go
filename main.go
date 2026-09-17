package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"brscan-go/scanner"
	"github.com/adrg/xdg"
	"github.com/brutella/dnssd"
	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
)

type Config struct {
	Scanner, Name, Dir, State string
	DPI                       int
	Now                       bool
}

func main() {
	var c Config
	flag.StringVar(&c.Scanner, "scanner", "", "scanner IPv4 address (default: mDNS discovery)")
	flag.StringVar(&c.Name, "name", "Brother Scan", "HomeKit accessory name")
	flag.StringVar(&c.Dir, "dir", "scans", "output directory")
	flag.StringVar(&c.State, "state", filepath.Join(xdg.StateHome, "brscan-go"), "persistent HomeKit pairing directory")
	flag.IntVar(&c.DPI, "dpi", 300, "resolution (validated against scanner capabilities)")
	flag.BoolVar(&c.Now, "now", false, "scan a color PDF immediately without starting HomeKit")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, c); err != nil && ctx.Err() == nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, c Config) error {
	endpoint := "http://" + c.Scanner + "/eSCL"
	if c.Scanner == "" {
		log.Print("discovering Brother scanners for 5s")
		// dnssd's readers recognize Canceled, not DeadlineExceeded.
		discoveryCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
		timer := time.AfterFunc(5*time.Second, stop)
		stopParent := context.AfterFunc(ctx, stop)
		found := map[string]string{}
		err := dnssd.LookupType(discoveryCtx, "_uscan._tcp.local.", func(e dnssd.BrowseEntry) {
			if !strings.Contains(strings.ToLower(e.Text["ty"]+e.Name), "brother") {
				return
			}
			for _, ip := range e.IPs {
				if ip.To4() != nil {
					path := e.Text["rs"]
					if path == "" {
						path = "eSCL"
					}
					u := url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), fmt.Sprint(e.Port)), Path: "/" + strings.TrimLeft(path, "/")}
					found[ip.String()] = u.String()
				}
			}
		}, func(dnssd.BrowseEntry) {})
		stop()
		timer.Stop()
		stopParent()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(found) == 0 {
			return fmt.Errorf("no Brother scanner discovered (%v); run on the scanner's LAN or set -scanner", err)
		}
		if len(found) > 1 {
			return fmt.Errorf("multiple scanners discovered: %v; select one with -scanner", found)
		}
		for c.Scanner, endpoint = range found {
		}
	}
	if ip := net.ParseIP(c.Scanner); ip.To4() == nil {
		return fmt.Errorf("scanner must be an IPv4 address")
	}
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return err
	}
	log.Printf("scanner %s; output %s", endpoint, c.Dir)
	scan := func(ctx context.Context, mode string) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		dir, err := scanner.Scan(ctx, scanner.Config{URL: endpoint, Dir: c.Dir, DPI: c.DPI, Color: mode})
		if err == nil {
			log.Printf("saved %s", dir)
		}
		return err
	}
	if c.Now {
		return scan(ctx, "RGB24")
	}
	if err := os.MkdirAll(c.State, 0700); err != nil {
		return err
	}
	store := hap.NewFsStore(c.State)
	pin, err := store.Get("pin")
	if errors.Is(err, os.ErrNotExist) {
		for {
			n, err := rand.Int(rand.Reader, big.NewInt(100000000))
			if err != nil {
				return err
			}
			pin = []byte(fmt.Sprintf("%08d", n.Int64()))
			if !hap.InvalidPins[string(pin)] {
				break
			}
		}
		if err := store.Set("pin", pin); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := strconv.Atoi(string(pin)); err != nil || len(pin) != 8 || hap.InvalidPins[string(pin)] {
		return fmt.Errorf("invalid pairing PIN in %s", c.State)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type request struct {
		switchAccessory *accessory.Switch
		mode            string
	}
	requests := make(chan request, 1)
	var busy atomic.Bool
	bridge := accessory.NewBridge(accessory.Info{Name: c.Name, Manufacturer: "brscan-go", SerialNumber: "bridge"})
	bridge.Id = 1
	accessories := []*accessory.A{}
	for i, profile := range []struct{ name, mode string }{
		{"Scan Color", "RGB24"}, {"Scan Text", "BlackAndWhite1"},
	} {
		a := accessory.NewSwitch(accessory.Info{Name: profile.name, Manufacturer: "brscan-go", SerialNumber: profile.mode})
		a.Id = uint64(i + 2) // Stable IDs across restarts.
		a.Switch.On.SetValueRequestFunc = func(value interface{}, _ *http.Request) (interface{}, int) {
			if ctx.Err() != nil {
				return nil, -70402 // Unable to communicate.
			}
			if !value.(bool) {
				if busy.Load() {
					return nil, -70403 // Off doesn't cancel an active scan.
				}
				return nil, 0
			}
			if !busy.CompareAndSwap(false, true) {
				return nil, -70403 // Resource busy: no queued/overlapping scans.
			}
			return nil, 0
		}
		// This callback runs after HAP has set the characteristic to On.
		a.Switch.On.OnValueRemoteUpdate(func(on bool) {
			if on {
				requests <- request{a, profile.mode}
			}
		})
		accessories = append(accessories, a.A)
	}
	srv, err := hap.NewServer(store, bridge.A, accessories...)
	if err != nil {
		return err
	}
	srv.Pin = string(pin)
	if !srv.IsPaired() {
		fmt.Printf("HomeKit pairing code: %s-%s-%s\n", pin[:3], pin[3:5], pin[5:])
	}
	log.Printf("HomeKit %q: pair using Home Assistant's HomeKit Device integration", c.Name)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-requests:
				if err := scan(ctx, req.mode); err != nil {
					log.Printf("scan failed: %v", err)
				}
				req.switchAccessory.Switch.On.SetValue(false)
				busy.Store(false)
			}
		}
	}()
	defer func() { cancel(); <-done }()
	return srv.ListenAndServe(ctx)
}
