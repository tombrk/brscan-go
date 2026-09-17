package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"brscan-go/brscankey"
	"brscan-go/scanner"
	"github.com/brutella/dnssd"
)

type Config struct {
	Scanner, Advertise, Name, Dir string
	DPI                           int
}

func main() {
	var c Config
	flag.StringVar(&c.Scanner, "scanner", "", "scanner IPv4 address (default: mDNS discovery)")
	flag.StringVar(&c.Advertise, "advertise", "", "local LAN IPv4 address (default: route to scanner)")
	flag.StringVar(&c.Name, "name", "Scan", "destination prefix (adds Text and Color)")
	flag.StringVar(&c.Dir, "dir", "scans", "output directory")
	flag.IntVar(&c.DPI, "dpi", 300, "resolution (validated against scanner capabilities)")
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
		discoveryCtx, stop := context.WithTimeout(ctx, 5*time.Second)
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
		}, nil)
		stop()
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
	if c.Advertise == "" {
		conn, err := net.Dial("udp4", net.JoinHostPort(c.Scanner, "161"))
		if err != nil {
			return err
		}
		c.Advertise = conn.LocalAddr().(*net.UDPAddr).IP.String()
		conn.Close()
	}
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return err
	}
	log.Printf("scanner %s; callback IP %s; output %s", endpoint, c.Advertise, c.Dir)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	text, color := make(chan struct{}), make(chan struct{}) // No queued scans.
	keyErrors := make(chan error, 2)
	var listeners sync.WaitGroup
	listeners.Go(func() {
		keyErrors <- brscankey.Notify(ctx, text, brscankey.Config{
			Scanner: c.Scanner, Host: c.Advertise, Name: c.Name + " Text",
		})
	})
	listeners.Go(func() {
		keyErrors <- brscankey.Notify(ctx, color, brscankey.Config{
			Scanner: c.Scanner, Host: c.Advertise, Name: c.Name + " Color",
		})
	})
	defer func() { cancel(); listeners.Wait() }()
	for {
		var mode string
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-keyErrors:
			return err
		case <-text:
			mode = "BlackAndWhite1"
		case <-color:
			mode = "RGB24"
		}
		scanCtx, stop := context.WithTimeout(ctx, 10*time.Minute)
		dir, err := scanner.Scan(scanCtx, scanner.Config{URL: endpoint, Dir: c.Dir, DPI: c.DPI, Color: mode})
		stop()
		if err != nil {
			log.Printf("scan failed: %v", err)
		} else {
			log.Printf("saved %s", dir)
		}
	}
}
