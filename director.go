package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"
)

const (
	apiVersion = "1.32"
	ownerKey   = "com.buildkite.safety-sock.owner"
)

var (
	versionRegex = regexp.MustCompile(`^/v\d\.\d+\b`)
)

type rulesDirector struct {
	Client *http.Client
	Owner  string
}

func (r *rulesDirector) Direct(l *log.Logger, req *http.Request, upstream http.Handler) http.Handler {
	var match = func(method string, pattern string) bool {
		if method != "*" && method != req.Method {
			return false
		}
		path := req.URL.Path
		if versionRegex.MatchString(path) {
			path = versionRegex.ReplaceAllString(path, "")
		}
		re := regexp.MustCompile(pattern)
		return re.MatchString(path)
	}

	var errorHandler = func(msg string, code int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			l.Printf("Handler returned error %q", msg)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"message": msg,
			})
			return
		})
	}

	switch {
	case match(`GET`, `^/(_ping|version|info)$`):
		return upstream

	// Container related endpoints
	case match(`POST`, `^/containers/create$`):
		return r.addLabelsToBody(l, req, upstream)
	case match(`POST`, `^/containers/prune$`):
		return r.addLabelsToQueryStringFilters(l, req, upstream)
	case match(`GET`, `^/containers/json$`):
		return r.addLabelsToQueryStringFilters(l, req, upstream)
	case match(`*`, `^/(containers|exec)/(\w+)\b`):
		if ok, err := r.checkOwner(l, "containers", false, req); ok {
			return upstream
		} else if err == errInspectNotFound {
			l.Printf("Container not found, allowing")
			return upstream
		} else if err != nil {
			return errorHandler(err.Error(), http.StatusInternalServerError)
		}
		return errorHandler("Unauthorized access to container", http.StatusUnauthorized)

	// Build related endpoints
	case match(`POST`, `^/build$`):
		return r.addLabelsToQueryStringLabels(l, req, upstream)

	// Image related endpoints
	case match(`GET`, `^/images/json$`):
		return r.addLabelsToQueryStringFilters(l, req, upstream)
	case match(`POST`, `^/images/create$`):
		return upstream
	case match(`POST`, `^/images/(create|search|get|load)$`):
		break
	case match(`POST`, `^/images/prune$`):
		return r.addLabelsToQueryStringFilters(l, req, upstream)
	case match(`*`, `^/images/(\w+)\b`):
		if ok, err := r.checkOwner(l, "images", true, req); ok {
			return upstream
		} else if err == errInspectNotFound {
			l.Printf("Image not found, allowing")
			return upstream
		} else if err != nil {
			return errorHandler(err.Error(), http.StatusInternalServerError)
		}
		return errorHandler("Unauthorized access to image", http.StatusUnauthorized)

	// Network related endpoints
	case match(`GET`, `^/networks$`):
		return r.addLabelsToQueryStringFilters(l, req, upstream)
	case match(`POST`, `^/networks/create$`):
		return r.addLabelsToBody(l, req, upstream)
	case match(`POST`, `^/networks/prune$`):
		return r.addLabelsToQueryStringFilters(l, req, upstream)
	case match(`GET`, `^/networks/(\w+)$`),
		match(`DELETE`, `^/networks/(\w+)$`),
		match(`POST`, `^/networks/(\w+)/(connect|disconnect)$`):
		if ok, err := r.checkOwner(l, "networks", true, req); ok {
			return upstream
		} else if err == errInspectNotFound {
			l.Printf("Network not found, allowing")
			return upstream
		} else if err != nil {
			return errorHandler(err.Error(), http.StatusInternalServerError)
		}
		return errorHandler("Unauthorized access to network", http.StatusUnauthorized)

	// Volumes related endpoints
	case match(`GET`, `^/volumes$`):
		return r.addLabelsToQueryStringFilters(l, req, upstream)
	case match(`POST`, `^/volumes/create$`):
		return r.addLabelsToBody(l, req, upstream)
	case match(`POST`, `^/volumes/prune$`):
		return r.addLabelsToQueryStringFilters(l, req, upstream)
	case match(`GET`, `^/volumes/(\w+)$`), match(`DELETE`, `^/volumes/(\w+)$`):
		if ok, err := r.checkOwner(l, "volumes", true, req); ok {
			return upstream
		} else if err == errInspectNotFound {
			l.Printf("Volume not found, allowing")
			return upstream
		} else if err != nil {
			return errorHandler(err.Error(), http.StatusInternalServerError)
		}
		return errorHandler("Unauthorized access to volume", http.StatusUnauthorized)

	}

	return errorHandler(req.Method+" "+req.URL.Path+" not implemented yet", http.StatusNotImplemented)
}

var identifierPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^/containers/(\w+?)(?:/\w+)?$`),
	regexp.MustCompile(`^/networks/(\w+?)(?:/\w+)?$`),
	regexp.MustCompile(`^/volumes/(\w+?)(?:/\w+)?$`),
	regexp.MustCompile(`^/images/(.+?)/(?:json|history|push|tag)$`),
	regexp.MustCompile(`^/images/([^/]+)$`),
	regexp.MustCompile(`^/images/(\w+/[^/]+)$`),
}

// Check owner takes a request for /vx.x/{kind}/{id} and uses inspect to see if it's
// got the correct owner label.
func (r *rulesDirector) checkOwner(l *log.Logger, kind string, allowEmpty bool, req *http.Request) (bool, error) {
	path := req.URL.Path
	if versionRegex.MatchString(path) {
		path = versionRegex.ReplaceAllString(path, "")
	}

	var identifier string

	for _, re := range identifierPatterns {
		if m := re.FindStringSubmatch(path); len(m) > 0 {
			log.Printf("%v %v", re, m)
			identifier = m[1]
		}
	}

	if identifier == "" {
		return false, fmt.Errorf("Unable to find an identifier in %s", path)
	}

	l.Printf("Looking up %s", identifier)

	labels, err := r.inspectLabels(kind, identifier)
	if err != nil {
		return false, err
	}

	l.Printf("Labels for %s: %v", path, labels)

	if val, exists := labels[ownerKey]; exists && val == r.Owner {
		l.Printf("Allow, %s matches owner %q", path, r.Owner)
		return true, nil
	} else if !exists && allowEmpty {
		l.Printf("Allow, %s has no owner", path)
		return true, nil
	} else {
		l.Printf("Deny, %s has owner %q, wanted %q", path, val, r.Owner)
		return false, nil
	}
}

func (r *rulesDirector) addLabelsToBody(l *log.Logger, req *http.Request, upstream http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		err := modifyRequestBody(req, func(decoded map[string]interface{}) {
			var labels = map[string]string{
				ownerKey: r.Owner,
			}
			l.Printf("Adding labels %v to body %v", labels, decoded["Labels"])
			for k, v := range labels {
				switch t := decoded["Labels"].(type) {
				case map[string]interface{}:
					t[k] = v
				default:
					l.Printf("Found %T instead", decoded["Labels"])
				}
			}
		})
		if err != nil {
			l.Printf("Err: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		upstream.ServeHTTP(w, req)
	})
}

func (r *rulesDirector) addLabelsToQueryStringFilters(l *log.Logger, req *http.Request, upstream http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		err := modifyRequestFilters(req, func(filters map[string][]interface{}) {
			label := ownerKey + "=" + r.Owner
			l.Printf("Adding label %v to label filters %v", label, filters["label"])
			filters["label"] = []interface{}{label}
			for _, val := range filters["label"] {
				if valString, ok := val.(string); ok {
					if valString != ownerKey && !strings.HasPrefix(valString, ownerKey+"=") {
						filters["label"] = append(filters["label"], valString)
					}
				}
			}
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		upstream.ServeHTTP(w, req)
	})
}

func (r *rulesDirector) addLabelsToQueryStringLabels(l *log.Logger, req *http.Request, upstream http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		l.Printf("Adding label %s=%s to querystring: %s %s",
			ownerKey, r.Owner, req.URL.Path, req.URL.RawQuery)

		var q = req.URL.Query()
		var labels = map[string]string{}

		if encoded := q.Get("labels"); encoded != "" {
			if err := json.NewDecoder(strings.NewReader(encoded)).Decode(&labels); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}

		labels[ownerKey] = r.Owner

		encoded, err := json.Marshal(labels)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		q.Set("labels", string(encoded))
		req.URL.RawQuery = q.Encode()

		upstream.ServeHTTP(w, req)
	})
}

var errInspectNotFound = errors.New("Not found")

func (r *rulesDirector) getInto(into interface{}, path string, arg ...interface{}) error {
	u := fmt.Sprintf("http://docker/v%s%s", apiVersion, fmt.Sprintf(path, arg...))

	resp, err := r.Client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return errInspectNotFound
	} else if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Request to %q failed: %s", u, resp.Status)
	}

	return json.NewDecoder(resp.Body).Decode(into)
}

func (r *rulesDirector) inspectLabels(kind, id string) (map[string]string, error) {
	switch kind {
	case "containers", "images":
		var result struct {
			Config struct {
				Labels map[string]string
			}
		}

		if err := r.getInto(&result, "/"+kind+"/%s/json", id); err != nil {
			return nil, err
		}

		return result.Config.Labels, nil
	case "networks", "volumes":
		var result struct {
			Labels map[string]string
		}

		if err := r.getInto(&result, "/"+kind+"/%s", id); err != nil {
			return nil, err
		}

		return result.Labels, nil
	}

	return nil, fmt.Errorf("Unknown kind %q", kind)
}

func modifyRequestFilters(req *http.Request, f func(filters map[string][]interface{})) error {
	var filters = map[string][]interface{}{}
	var q = req.URL.Query()

	if encoded := q.Get("filters"); encoded != "" {
		var generic map[string]interface{}

		log.Printf("filters=%q", encoded)
		if err := json.NewDecoder(strings.NewReader(encoded)).Decode(&generic); err != nil {
			return err
		}
		for k, v := range generic {
			switch tv := v.(type) {
			case map[string]interface{}:
				for mk, mv := range tv {
					log.Printf("Adding %s = %v", mk, mv)
					filters[k] = []interface{}{mk}
				}
			default:
				log.Printf("[%s] Got type %T: %v", k, v, tv)
			}
		}

		log.Printf("%#v", filters)
	}

	f(filters)

	encoded, err := json.Marshal(filters)
	if err != nil {
		return err
	}

	q.Set("filters", string(encoded))
	req.URL.RawQuery = q.Encode()
	return nil
}

func modifyRequestBody(req *http.Request, f func(filters map[string]interface{})) error {
	var decoded map[string]interface{}

	if err := json.NewDecoder(req.Body).Decode(&decoded); err != nil {
		return err
	}

	f(decoded)

	encoded, err := json.Marshal(decoded)
	if err != nil {
		return err
	}

	// reset it so that upstream can read it again
	req.ContentLength = int64(len(encoded))
	req.Body = ioutil.NopCloser(bytes.NewReader(encoded))

	return nil
}