package scanner

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScan(t *testing.T) {
	for _, tc := range []struct {
		name, adf, source, color string
		dpi, downloadStatus      int
		wantError                bool
	}{
		{"color feeder", "ScannerAdfLoaded", "Feeder", "RGB24", 300, 200, false},
		{"text flatbed", "ScannerAdfEmpty", "Platen", "BlackAndWhite1", 300, 200, false},
		{"unsupported DPI", "ScannerAdfLoaded", "Feeder", "RGB24", 123, 200, true},
		{"gone is failure", "ScannerAdfLoaded", "Feeder", "RGB24", 300, 410, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created, downloads := false, 0
			pdf := "%PDF-1.4\ntransfer test fixture\n%%EOF\n"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /eSCL/ScannerStatus":
					fmt.Fprintf(w, "<ScannerStatus><State>Idle</State><AdfState>%s</AdfState></ScannerStatus>", tc.adf)
				case "GET /eSCL/ScannerCapabilities":
					profile := `<MaxWidth>2550</MaxWidth><MaxHeight>3507</MaxHeight><SettingProfiles><SettingProfile><ColorModes><ColorMode>RGB24</ColorMode><ColorMode>BlackAndWhite1</ColorMode></ColorModes><DocumentFormats><DocumentFormat>application/pdf</DocumentFormat></DocumentFormats><SupportedResolutions><DiscreteResolutions><DiscreteResolution><XResolution>300</XResolution><YResolution>300</YResolution></DiscreteResolution><DiscreteResolution><XResolution>600</XResolution><YResolution>600</YResolution></DiscreteResolution></DiscreteResolutions></SupportedResolutions></SettingProfile></SettingProfiles>`
					fmt.Fprintf(w, "<ScannerCapabilities><Platen><PlatenInputCaps>%s</PlatenInputCaps></Platen><Adf><AdfSimplexInputCaps>%s</AdfSimplexInputCaps></Adf></ScannerCapabilities>", profile, profile)
				case "POST /eSCL/ScanJobs":
					created = true
					var settings struct {
						InputSource, ColorMode, DocumentFormat string
						XResolution, YResolution               int
						Duplex                                 bool
					}
					if err := xml.NewDecoder(r.Body).Decode(&settings); err != nil {
						t.Error(err)
					}
					if settings.InputSource != tc.source || settings.ColorMode != tc.color || settings.DocumentFormat != "application/pdf" || settings.XResolution != tc.dpi || settings.YResolution != tc.dpi || settings.Duplex {
						t.Errorf("unexpected settings: %+v", settings)
					}
					w.Header().Set("Location", "/eSCL/ScanJobs/test")
					w.WriteHeader(http.StatusCreated)
				case "GET /eSCL/ScanJobs/test/NextDocument":
					if downloads == 2 {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					downloads++
					w.WriteHeader(tc.downloadStatus)
					if tc.downloadStatus == 200 {
						fmt.Fprint(w, pdf)
					}
				case "DELETE /eSCL/ScanJobs/test":
					w.WriteHeader(http.StatusNotFound)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer srv.Close()
			dir, err := Scan(context.Background(), Config{URL: srv.URL + "/eSCL", Dir: t.TempDir(), DPI: tc.dpi, Color: tc.color})
			if (err != nil) != tc.wantError {
				t.Fatalf("Scan() error = %v", err)
			}
			if tc.dpi == 123 && created {
				t.Fatal("unsupported DPI reached job creation")
			}
			if tc.downloadStatus == 410 && !strings.HasSuffix(dir, ".partial") {
				t.Fatal("failed download marked complete")
			}
			if tc.wantError {
				return
			}
			if strings.HasSuffix(dir, ".partial") {
				t.Fatal("successful job still partial")
			}
			for i := 1; i <= 2; i++ {
				data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%03d.pdf", i)))
				if err != nil || string(data) != pdf {
					t.Fatalf("PDF %d changed or missing: %v", i, err)
				}
			}
		})
	}
}
