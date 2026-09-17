// Package brscankey registers a Brother Scan-to-PC destination and receives button events.
package brscankey

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

type Config struct {
	Scanner, Host, Name string
}

// Notify registers one destination on its own UDP port and signals events on
// button presses. Sends are nonblocking; busy consumers drop events. It runs
// until ctx is cancelled and never closes events. Registration expires in 5m.
func Notify(ctx context.Context, events chan<- struct{}, c Config) error {
	ip, host := net.ParseIP(c.Scanner), net.ParseIP(c.Host)
	if ip.To4() == nil || host.To4() == nil || host.IsUnspecified() {
		return fmt.Errorf("scanner and advertised host must be IPv4 addresses")
	}
	if events == nil {
		return fmt.Errorf("notification channel is nil")
	}
	if c.Name == "" || len(c.Name) > 15 || strings.ContainsAny(c.Name, ";\"\r\n") {
		return fmt.Errorf("destination name must be 1–15 bytes without quotes or semicolons")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	snmp := &gosnmp.GoSNMP{
		Target: c.Scanner, Port: 161, Version: gosnmp.Version1,
		Community: "internal", Timeout: 2 * time.Second, Retries: 1, Context: ctx,
	}
	if err := snmp.Connect(); err != nil {
		return err
	}
	defer snmp.Conn.Close()
	callback := (&net.UDPAddr{IP: host, Port: conn.LocalAddr().(*net.UDPAddr).Port}).String()
	value := fmt.Sprintf("TYPE=BR;BUTTON=SCAN;USER=\"%s\";FUNC=FILE;HOST=%s;APPNUM=5;DURATION=300;BRID=;", c.Name, callback)
	var refresh time.Time
	seen := map[string]time.Time{}
	buf := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(refresh) {
			packet, err := snmp.Set([]gosnmp.SnmpPDU{{
				Name: "1.3.6.1.4.1.2435.2.3.9.2.11.1.1.0", Type: gosnmp.OctetString, Value: value,
			}})
			if err != nil {
				log.Printf("%s registration failed (retry in 5s): %v", c.Name, err)
				refresh = time.Now().Add(5 * time.Second)
			} else if packet.Error != gosnmp.NoError {
				return fmt.Errorf("%s registration: SNMP %s", c.Name, packet.Error)
			} else {
				log.Printf("registered %q at %s", c.Name, callback)
				refresh = time.Now().Add(2 * time.Minute)
			}
			for key, at := range seen {
				if time.Since(at) > 5*time.Minute {
					delete(seen, key)
				}
			}
		}
		if err := conn.SetReadDeadline(refresh); err != nil {
			return err
		}
		n, sender, err := conn.ReadFromUDP(buf)
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Timeout() {
				continue
			}
			return err
		}
		if !sender.IP.Equal(ip) {
			continue
		}
		text := string(buf[:n])
		start := strings.Index(text, "TYPE=BR;")
		if start < 0 {
			continue
		}
		fields := map[string]string{}
		for _, field := range strings.Split(text[start:], ";") {
			key, value, ok := strings.Cut(field, "=")
			if ok {
				fields[key] = strings.Trim(value, "\"")
			}
		}
		if fields["BUTTON"] != "SCAN" || fields["FUNC"] != "FILE" || fields["APPNUM"] != "5" ||
			fields["USER"] != c.Name || fields["HOST"] != callback || fields["SEQ"] == "" {
			continue
		}
		key := fields["REGID"] + ":" + fields["SEQ"]
		if at, ok := seen[key]; ok && time.Since(at) < 5*time.Minute {
			continue
		}
		seen[key] = time.Now()
		select {
		case events <- struct{}{}:
			log.Printf("scan button pressed: %s", fields["USER"])
		default:
			log.Print("scan button ignored: busy")
		}
	}
}
