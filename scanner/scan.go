// Package scanner saves auto-source AirScan jobs as PDFs on disk.
package scanner

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stapelberg/airscan"
	"github.com/stapelberg/airscan/preset"
)

type Config struct {
	URL, Dir, Color string
	DPI             int
}

// airscan has no context API and leaves response bodies open. This sequential
// adapter attaches our context and closes the previous body before each request.
type client struct {
	ctx context.Context
	http.Client
	body io.ReadCloser
	base *url.URL
}

func (c *client) Do(req *http.Request) (*http.Response, error) {
	if c.body != nil {
		c.body.Close()
	}
	req = req.Clone(c.ctx)
	// airscan hardcodes /eSCL; honor the endpoint advertised by mDNS.
	req.URL.Scheme, req.URL.Host = c.base.Scheme, c.base.Host
	req.URL.Path = strings.TrimRight(c.base.Path, "/") + strings.TrimPrefix(req.URL.Path, "/eSCL")
	resp, err := c.Client.Do(req)
	if resp != nil {
		c.body = resp.Body
	}
	return resp, err
}

// Scan requests PDFs, not images. Each NextDocument response is saved verbatim:
// some scanners return one PDF per page, others return a multi-page PDF.
// A job directory loses its .partial suffix only after successful completion.
// Failed jobs retain downloaded files for recovery; they are never retried automatically.
func Scan(ctx context.Context, cfg Config) (string, error) {
	base, err := url.Parse(cfg.URL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return "", fmt.Errorf("invalid scanner URL %q", cfg.URL)
	}
	h := &client{ctx: ctx, base: base, Client: http.Client{
		Timeout:       2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	defer func() {
		if h.body != nil {
			h.body.Close()
		}
	}()
	c := airscan.NewClient(base.Host)
	c.HTTPClient = h
	status, err := c.ScannerStatus()
	if err != nil {
		return "", err
	}
	if status.State != "Idle" {
		return "", fmt.Errorf("scanner is %s", status.State)
	}
	source := "Platen"
	switch status.ADFState {
	case "ScannerAdfLoaded":
		source = "Feeder"
	case "ScannerAdfEmpty":
	default:
		return "", fmt.Errorf("cannot auto-select source: ADF state %q", status.ADFState)
	}
	// Upstream's capability model uses singular fields for repeated profiles
	// and resolutions. Decode those fields ourselves; acquisition is all airscan.
	type input struct {
		MaxWidth, MaxHeight int
		Profiles            []struct {
			Formats     []string                                 `xml:"DocumentFormats>DocumentFormat"`
			FormatsExt  []string                                 `xml:"DocumentFormats>DocumentFormatExt"`
			Colors      []string                                 `xml:"ColorModes>ColorMode"`
			Resolutions []struct{ XResolution, YResolution int } `xml:"SupportedResolutions>DiscreteResolutions>DiscreteResolution"`
		} `xml:"SettingProfiles>SettingProfile"`
	}
	var caps struct {
		Platen input `xml:"Platen>PlatenInputCaps"`
		Feeder input `xml:"Adf>AdfSimplexInputCaps"`
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+base.Host+"/eSCL/ScannerCapabilities", nil)
	if err != nil {
		return "", err
	}
	resp, err := h.Do(req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("capabilities: %s", resp.Status)
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&caps); err != nil {
		return "", err
	}
	in := caps.Platen
	if source == "Feeder" {
		in = caps.Feeder
	}
	supported := false
	for _, p := range in.Profiles {
		if !slices.Contains(p.Formats, "application/pdf") && !slices.Contains(p.FormatsExt, "application/pdf") {
			continue
		}
		for _, r := range p.Resolutions {
			if r.XResolution != cfg.DPI || r.YResolution != cfg.DPI {
				continue
			}
			if slices.Contains(p.Colors, cfg.Color) {
				supported = true
			}
		}
	}
	if !supported || in.MaxWidth <= 0 || in.MaxHeight <= 0 {
		return "", fmt.Errorf("%s does not advertise PDF with %s at %d DPI", source, cfg.Color, cfg.DPI)
	}
	settings := preset.GrayscaleA4ADF()
	settings.InputSource, settings.DocumentFormat, settings.ColorMode = source, "application/pdf", cfg.Color
	settings.XResolution, settings.YResolution, settings.Duplex = cfg.DPI, cfg.DPI, false
	// Keep the preset's A4 region, bounded by this source's advertised limits.
	settings.ScanRegions.Regions[0].Width = min(settings.ScanRegions.Regions[0].Width, in.MaxWidth)
	settings.ScanRegions.Regions[0].Height = min(settings.ScanRegions.Regions[0].Height, in.MaxHeight)
	if err := os.MkdirAll(cfg.Dir, 0700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(cfg.Dir, time.Now().Format("2006-01-02_150405")+"-*.partial")
	if err != nil {
		return "", err
	}
	log.Printf("scanning %s, %d DPI, %s → %s", source, cfg.DPI, cfg.Color, dir)
	job, err := c.Scan(settings)
	if err != nil {
		return dir, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		h.ctx = cleanup
		if err := job.Close(); err != nil {
			log.Printf("scan job cleanup: %v", err)
		}
	}()
	count := 0
	for job.ScanPage() {
		count++
		name := filepath.Join(dir, fmt.Sprintf("%03d.pdf", count))
		f, err := os.OpenFile(name+".partial", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return dir, err
		}
		page := job.CurrentPage()
		header := make([]byte, 5)
		_, err = io.ReadFull(page, header)
		if err == nil && string(header) != "%PDF-" {
			err = fmt.Errorf("scanner returned non-PDF data")
		}
		if err == nil {
			_, err = f.Write(header)
		}
		var n int64
		if err == nil {
			n, err = io.Copy(f, io.LimitReader(page, (512<<20)+1))
			if err == nil && n > 512<<20 {
				err = fmt.Errorf("PDF exceeds 512 MiB limit")
			}
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return dir, err
		}
		if closeErr != nil {
			return dir, closeErr
		}
		if err := os.Rename(name+".partial", name); err != nil {
			return dir, err
		}
		log.Printf("received %s (%d bytes)", name, n+5)
	}
	if err := job.Err(); err != nil {
		return dir, err
	}
	if count == 0 {
		return dir, fmt.Errorf("scanner returned no PDFs")
	}
	complete := strings.TrimSuffix(dir, ".partial")
	if err := os.Rename(dir, complete); err != nil {
		return dir, err
	}
	return complete, nil
}
