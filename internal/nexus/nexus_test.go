package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeNexus is a minimal Nexus 3 REST API stand-in.
type fakeNexus struct {
	settingsStatus int // status for /v1/repositorySettings (200 = serve)
	sawAuth        string
}

func (f *fakeNexus) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositorySettings", func(w http.ResponseWriter, r *http.Request) {
		f.sawAuth = r.Header.Get("Authorization")
		if f.settingsStatus != 0 && f.settingsStatus != 200 {
			w.WriteHeader(f.settingsStatus)
			return
		}
		fmt.Fprint(w, `[
			{"name":"maven-hosted","format":"maven2","type":"hosted","attributes":{"storage":{"blobStoreName":"default"}}},
			{"name":"npm-proxy","format":"npm","type":"proxy","attributes":{"proxy":{"remoteUrl":"https://registry.npmjs.org"}}},
			{"name":"maven-all","format":"maven2","type":"group","attributes":{"group":{"memberNames":["maven-hosted"]}}}
		]`)
	})
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"name":"maven-hosted","format":"maven2","type":"hosted","url":"x"}]`)
	})
	mux.HandleFunc("/service/rest/v1/components", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("repository") != "maven-hosted" {
			http.Error(w, "unknown repo", 404)
			return
		}
		token := r.URL.Query().Get("continuationToken")
		switch token {
		case "":
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"id": "c1", "name": "org.acme:app", "version": "1.0",
						"assets": []map[string]any{{"path": "org/acme/app/1.0/app-1.0.jar", "downloadUrl": "http://x/1"}}},
				},
				"continuationToken": "page2",
			})
		case "page2":
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"id": "c2", "name": "org.acme:lib", "version": "2.0",
						"assets": []map[string]any{
							{"path": "org/acme/lib/2.0/lib-2.0.jar", "downloadUrl": "http://x/2"},
							{"path": "org/acme/lib/2.0/lib-2.0.pom", "downloadUrl": "http://x/3"},
						}},
				},
				"continuationToken": "",
			})
		default:
			http.Error(w, "bad token", 400)
		}
	})
	mux.HandleFunc("/service/rest/v1/asset-bytes", func(w http.ResponseWriter, r *http.Request) {
		f.sawAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, "JARBYTES")
	})
	return mux
}

func TestListRepositories_Settings(t *testing.T) {
	f := &fakeNexus{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := New(srv.URL, "admin", "secret")
	repos, err := c.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 3 {
		t.Fatalf("got %d repos", len(repos))
	}
	byName := map[string]Repository{}
	for _, r := range repos {
		byName[r.Name] = r
	}
	if byName["npm-proxy"].RemoteURL != "https://registry.npmjs.org" {
		t.Fatalf("proxy remoteUrl not lifted: %+v", byName["npm-proxy"])
	}
	if got := byName["maven-all"].Members; len(got) != 1 || got[0] != "maven-hosted" {
		t.Fatalf("group members not lifted: %v", got)
	}
	if f.sawAuth == "" {
		t.Fatal("no basic auth sent")
	}
}

func TestListRepositories_FallbackOn403(t *testing.T) {
	f := &fakeNexus{settingsStatus: 403}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	repos, err := New(srv.URL, "u", "p").ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Name != "maven-hosted" {
		t.Fatalf("fallback failed: %+v", repos)
	}
}

func TestListComponents_Pagination(t *testing.T) {
	f := &fakeNexus{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	var comps []Component
	err := New(srv.URL, "u", "p").ListComponents(context.Background(), "maven-hosted", func(c Component) error {
		comps = append(comps, c)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(comps) != 2 {
		t.Fatalf("got %d components", len(comps))
	}
	nComp, nAssets, err := New(srv.URL, "u", "p").CountComponents(context.Background(), "maven-hosted")
	if err != nil || nComp != 2 || nAssets != 3 {
		t.Fatalf("counts = %d,%d,%v", nComp, nAssets, err)
	}
}

func TestDownload(t *testing.T) {
	f := &fakeNexus{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	body, _, err := New(srv.URL, "u", "p").Download(context.Background(), srv.URL+"/service/rest/v1/asset-bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	b, _ := io.ReadAll(body)
	if string(b) != "JARBYTES" {
		t.Fatalf("body = %q", b)
	}
	if f.sawAuth == "" {
		t.Fatal("download sent no auth")
	}

	_, _, err = New(srv.URL, "u", "p").Download(context.Background(), srv.URL+"/nope")
	if !IsStatus(err, 404) {
		t.Fatalf("want APIError 404, got %v", err)
	}
}
