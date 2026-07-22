package upnp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseSSDPLocation(t *testing.T) {
	cases := []struct {
		name    string
		resp    string
		want    string
		wantErr bool
	}{
		{
			name: "typical response, uppercase header",
			resp: "HTTP/1.1 200 OK\r\n" +
				"CACHE-CONTROL: max-age=1800\r\n" +
				"LOCATION: http://192.168.1.1:5000/rootDesc.xml\r\n" +
				"ST: urn:schemas-upnp-org:service:WANIPConnection:1\r\n\r\n",
			want: "http://192.168.1.1:5000/rootDesc.xml",
		},
		{
			name: "mixed-case header, as some implementations send it",
			resp: "HTTP/1.1 200 OK\r\n" +
				"Location: http://10.0.0.1:1900/desc.xml\r\n\r\n",
			want: "http://10.0.0.1:1900/desc.xml",
		},
		{
			name:    "no LOCATION header at all",
			resp:    "HTTP/1.1 200 OK\r\nST: something\r\n\r\n",
			wantErr: true,
		},
		{
			name:    "empty LOCATION value",
			resp:    "HTTP/1.1 200 OK\r\nLOCATION: \r\n\r\n",
			wantErr: true,
		},
		{
			name:    "garbage, not an SSDP response at all",
			resp:    "not even close to a header block",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSSDPLocation([]byte(tc.resp))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got location %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A device description where the WAN service sits directly on the root
// device — some simpler/virtual IGDs are shaped this way even though most
// consumer routers nest it several levels down (see the deviceList case
// below).
const flatDeviceDescription = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
        <controlURL>/upnp/control/WANIPConn1</controlURL>
      </service>
    </serviceList>
  </device>
</root>`

// The realistic shape most consumer routers actually use: root device ->
// WANDevice -> WANConnectionDevice -> WANIPConnection service, several
// levels of deviceList nesting deep.
const nestedDeviceDescription = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
    <deviceList>
      <device>
        <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
        <deviceList>
          <device>
            <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
            <serviceList>
              <service>
                <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
                <controlURL>/ctl/IPConn</controlURL>
              </service>
            </serviceList>
          </device>
        </deviceList>
      </device>
    </deviceList>
  </device>
</root>`

const pppOnlyDeviceDescription = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:WANPPPConnection:1</serviceType>
        <controlURL>/ctl/PPPConn</controlURL>
      </service>
    </serviceList>
  </device>
</root>`

const noWANServiceDescription = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType>
        <controlURL>/ctl/L3F</controlURL>
      </service>
    </serviceList>
  </device>
</root>`

func TestParseDeviceDescription(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantURL     string
		wantService string
		wantErr     bool
	}{
		{"flat", flatDeviceDescription, "/upnp/control/WANIPConn1", "urn:schemas-upnp-org:service:WANIPConnection:1", false},
		{"nested several levels deep", nestedDeviceDescription, "/ctl/IPConn", "urn:schemas-upnp-org:service:WANIPConnection:1", false},
		{"PPP fallback when no WANIPConnection exists", pppOnlyDeviceDescription, "/ctl/PPPConn", "urn:schemas-upnp-org:service:WANPPPConnection:1", false},
		{"no WAN service at all", noWANServiceDescription, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cu, st, err := parseDeviceDescription([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got controlURL=%q serviceType=%q", cu, st)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cu != tc.wantURL || st != tc.wantService {
				t.Errorf("got (%q, %q), want (%q, %q)", cu, st, tc.wantURL, tc.wantService)
			}
		})
	}
}

func TestParseDeviceDescriptionPrefersIPOverPPP(t *testing.T) {
	// A device oddly advertising both should prefer WANIPConnection.
	both := `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:WANPPPConnection:1</serviceType>
        <controlURL>/ctl/PPPConn</controlURL>
      </service>
      <service>
        <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
        <controlURL>/ctl/IPConn</controlURL>
      </service>
    </serviceList>
  </device>
</root>`
	cu, st, err := parseDeviceDescription([]byte(both))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cu != "/ctl/IPConn" || st != "urn:schemas-upnp-org:service:WANIPConnection:1" {
		t.Errorf("got (%q, %q), want the IPConnection service preferred", cu, st)
	}
}

func TestResolveControlURL(t *testing.T) {
	descURL := mustParseURL(t, "http://192.168.1.1:5000/desc/rootDesc.xml")
	cases := []struct {
		path string
		want string
	}{
		{"/upnp/control/WANIPConn1", "http://192.168.1.1:5000/upnp/control/WANIPConn1"},
		{"ctl/IPConn", "http://192.168.1.1:5000/desc/ctl/IPConn"},
		{"http://192.168.1.1:5000/absolute/ctl", "http://192.168.1.1:5000/absolute/ctl"},
	}
	for _, tc := range cases {
		if got := resolveControlURL(descURL, tc.path); got != tc.want {
			t.Errorf("resolveControlURL(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// AddPortMapping and DeletePortMapping against a fake IGD control server:
// verifies the request is a well-formed SOAP call carrying the right
// SOAPAction/arguments, and that a plain success response (no Fault) is
// treated as success.
func TestGatewayAddAndDeletePortMappingSuccess(t *testing.T) {
	var gotAction string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAction = r.Header.Get("SOAPAction")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <u:AddPortMappingResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"></u:AddPortMappingResponse>
  </s:Body>
</s:Envelope>`)
	}))
	defer srv.Close()

	gw := &Gateway{
		ControlURL:  srv.URL,
		ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:1",
		LocalIP:     "192.168.1.50",
	}
	if err := gw.AddPortMapping(context.Background(), 65432, 65432, "UDP", "gravinet", 3600); err != nil {
		t.Fatalf("AddPortMapping: %v", err)
	}
	wantAction := `"urn:schemas-upnp-org:service:WANIPConnection:1#AddPortMapping"`
	if gotAction != wantAction {
		t.Errorf("SOAPAction = %q, want %q", gotAction, wantAction)
	}
	for _, want := range []string{
		"<NewExternalPort>65432</NewExternalPort>",
		"<NewProtocol>UDP</NewProtocol>",
		"<NewInternalPort>65432</NewInternalPort>",
		"<NewInternalClient>192.168.1.50</NewInternalClient>",
		"<NewEnabled>1</NewEnabled>",
		"<NewLeaseDuration>3600</NewLeaseDuration>",
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %q\n--- got ---\n%s", want, gotBody)
		}
	}
}

func TestGatewayDeletePortMappingSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <u:DeletePortMappingResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"></u:DeletePortMappingResponse>
  </s:Body>
</s:Envelope>`)
	}))
	defer srv.Close()

	gw := &Gateway{ControlURL: srv.URL, ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:1"}
	if err := gw.DeletePortMapping(context.Background(), 65432, "UDP"); err != nil {
		t.Fatalf("DeletePortMapping: %v", err)
	}
}

// A rejected request comes back as an HTTP 500 carrying a SOAP Fault, per
// SOAP 1.1 — the status code alone must not be treated as the failure
// signal; the UPnP error code in the fault body is what's actionable.
func TestGatewaySOAPFaultSurfacesUPnPErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <s:Fault>
      <faultcode>s:Client</faultcode>
      <faultstring>UPnPError</faultstring>
      <detail>
        <UPnPError xmlns="urn:schemas-upnp-org:control-1-0">
          <errorCode>718</errorCode>
          <errorDescription>ConflictInMappingEntry</errorDescription>
        </UPnPError>
      </detail>
    </s:Fault>
  </s:Body>
</s:Envelope>`)
	}))
	defer srv.Close()

	gw := &Gateway{ControlURL: srv.URL, ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:1", LocalIP: "192.168.1.50"}
	err := gw.AddPortMapping(context.Background(), 65432, 65432, "UDP", "gravinet", 3600)
	if err == nil {
		t.Fatal("expected an error from a SOAP Fault response, got nil")
	}
	if !strings.Contains(err.Error(), "718") || !strings.Contains(err.Error(), "ConflictInMappingEntry") {
		t.Errorf("error %q doesn't surface the UPnP error code/description", err.Error())
	}
}

func TestGatewayGetExternalIPAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">
      <NewExternalIPAddress>203.0.113.7</NewExternalIPAddress>
    </u:GetExternalIPAddressResponse>
  </s:Body>
</s:Envelope>`)
	}))
	defer srv.Close()

	gw := &Gateway{ControlURL: srv.URL, ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:1"}
	ip, err := gw.GetExternalIPAddress(context.Background())
	if err != nil {
		t.Fatalf("GetExternalIPAddress: %v", err)
	}
	if ip != "203.0.113.7" {
		t.Errorf("got %q, want 203.0.113.7", ip)
	}
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return u
}
