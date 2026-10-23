// mockcoolify is a DEVELOPMENT-ONLY fake of the few Coolify API v1 endpoints
// Dilreet uses. It checks auth, methods and required body fields as listed
// in Coolify's openapi.yaml, and logs every request. It is not part of the exe.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
)

var (
	mu       sync.Mutex
	projects = map[string]map[string]any{}
	apps     = map[string]map[string]any{}
	n        int
)

func id() string { n++; return fmt.Sprintf("uuid%04d", n) }

func main() {
	addr := flag.String("addr", "127.0.0.1:18000", "")
	token := flag.String("token", "testtoken", "")
	flag.Parse()
	h := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		log.Printf("%s %s %s", r.Method, r.URL.RequestURI(), b)
		if r.Header.Get("Authorization") != "Bearer "+*token {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message":"Unauthenticated."}`)
			return
		}
		need := func(fields ...string) bool {
			for _, f := range fields {
				if _, ok := body[f]; !ok {
					w.WriteHeader(422)
					fmt.Fprintf(w, `{"message":"missing %s"}`, f)
					return false
				}
			}
			return true
		}
		js := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(v)
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		switch {
		case r.Method == "GET" && p == "/version":
			fmt.Fprint(w, "4.4.7")
		case r.Method == "GET" && p == "/servers":
			js(200, []map[string]any{{"uuid": "srv1", "name": "localhost", "ip": "host.docker.internal"}})
		case r.Method == "GET" && p == "/projects":
			out := []any{}
			for _, v := range projects {
				out = append(out, v)
			}
			js(200, out)
		case r.Method == "POST" && p == "/projects":
			if !need("name") {
				return
			}
			u := id()
			projects[u] = map[string]any{"uuid": u, "name": body["name"], "description": body["description"]}
			js(201, map[string]any{"uuid": u})
		case r.Method == "DELETE" && strings.HasPrefix(p, "/projects/"):
			u := strings.TrimPrefix(p, "/projects/")
			for _, a := range apps {
				if a["project_uuid"] == u {
					js(400, map[string]any{"message": "Project has resources"})
					return
				}
			}
			delete(projects, u)
			js(200, map[string]any{"message": "Project deleted."})
		case r.Method == "POST" && p == "/applications/dockerimage":
			if !need("project_uuid", "server_uuid", "environment_name", "docker_registry_image_name") {
				return
			}
			u := id()
			body["uuid"] = u
			body["fqdn"] = "http://" + u + ".203.0.113.10.sslip.io"
			body["status"] = "exited"
			apps[u] = body
			js(201, map[string]any{"uuid": u})
		case r.Method == "GET" && p == "/applications":
			out := []any{}
			for _, v := range apps {
				out = append(out, v)
			}
			js(200, out)
		case strings.HasPrefix(p, "/applications/"):
			parts := strings.Split(strings.TrimPrefix(p, "/applications/"), "/")
			a, ok := apps[parts[0]]
			if !ok {
				js(404, map[string]any{"message": "Resource not found."})
				return
			}
			switch {
			case len(parts) == 1 && r.Method == "GET":
				js(200, a)
			case len(parts) == 1 && r.Method == "PATCH":
				if d, ok := body["domains"].(string); ok {
					a["fqdn"] = d
				}
				js(200, map[string]any{"uuid": parts[0]})
			case len(parts) == 1 && r.Method == "DELETE":
				delete(apps, parts[0])
				js(200, map[string]any{"message": "Application deletion request queued."})
			case len(parts) == 2 && parts[1] == "storages" && r.Method == "POST":
				if !need("type", "mount_path") {
					return
				}
				if body["type"] == "file" && body["is_directory"] == true && body["fs_path"] == nil {
					js(422, map[string]any{"message": "fs_path required"})
					return
				}
				a["storage"] = body
				js(201, map[string]any{"uuid": id()})
			case len(parts) == 2 && parts[1] == "restart" && r.Method == "POST":
				a["status"] = "running:healthy"
				js(200, map[string]any{"message": "Restart request queued."})
			default:
				js(405, map[string]any{"message": "method not allowed"})
			}
		case r.Method == "POST" && p == "/deploy":
			u := r.URL.Query().Get("uuid")
			if a, ok := apps[u]; ok {
				a["status"] = "running:healthy"
			}
			js(200, map[string]any{"deployments": []any{map[string]any{"message": "queued", "resource_uuid": u, "deployment_uuid": id()}}})
		default:
			js(404, map[string]any{"message": "not found"})
		}
	}
	log.Printf("mock Coolify on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(h)))
}
