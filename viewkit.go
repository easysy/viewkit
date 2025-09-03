package viewkit

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
)

//go:embed style.css main.gohtml
var viewkit embed.FS

const (
	inner = "inner"
	main  = "main"
	view  = "view"
)

const innerHTML = `{{ define "inner" }}
<!DOCTYPE html>
<html>
  <head>
    %s<link rel="stylesheet" href="/viewkit/style.css">%s
  </head>
  <body>
    {{- template "body" . }}
  </body>
</html>
{{ end }}`

func loader(readDir func(string) ([]fs.DirEntry, error), folder, suffix string) []string {
	entries, err := readDir(folder)
	if err != nil {
		return nil
	}

	out := make([]string, 0)

	for _, entry := range entries {
		name := entry.Name()

		if entry.IsDir() {
			loader(readDir, folder+"/"+name, suffix)
			continue
		}

		if strings.Contains(name, main) {
			continue
		}

		if strings.HasSuffix(name, suffix) {
			out = append(out, folder+"/"+name)
		}
	}

	return out
}

func loadStyles(static embed.FS) (styles string) {
	entries := loader(static.ReadDir, "static", ".css")
	for _, entry := range entries {
		styles += "\n\t" + "<link rel=\"stylesheet\" href=\"/" + entry + "\">"
	}
	return
}

func wrapTitle(s string) string {
	if s == "" {
		return s
	}
	return "<title>" + s + "</title>\n\t"
}

type Viewer interface {
	AddSource(name string, data func(*http.Request) any)
	AddView(name, tmpl string, data func(*http.Request) any)
	Inject(router *http.ServeMux)
}

type Configuration struct {
	Path      string
	Title     string
	StartView string
	FuncMap   template.FuncMap
}

func New(cfg Configuration, static, templates embed.FS) Viewer {
	cfg.Path = strings.Trim(path.Clean(cfg.Path), "/")
	cfg.FuncMap["basepath"] = func() string { return cfg.Path }
	return &viewer{
		cfg:       cfg,
		inner:     fmt.Sprintf(innerHTML, wrapTitle(cfg.Title), loadStyles(static)),
		views:     make(map[string]http.HandlerFunc),
		sources:   make(map[string]func(*http.Request) any),
		static:    static,
		templates: templates,
	}
}

type viewer struct {
	cfg       Configuration
	inner     string
	views     map[string]http.HandlerFunc
	sources   map[string]func(*http.Request) any
	static    embed.FS
	templates embed.FS
}

func (v *viewer) handler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Content-Request") != "true" {
		v.views[main](w, r)
		return
	}

	key := r.URL.Query().Get(view)
	if proc, ok := v.views[key]; ok {
		proc(w, r)
		return
	}

	http.Error(w, "Unknown view: "+key, http.StatusBadRequest)
}

func (v *viewer) addView(name string, parse func(*template.Template), data func(*http.Request) any) {
	t := template.Must(template.New(inner).Funcs(v.cfg.FuncMap).Parse(v.inner))
	parse(t)

	v.views[name] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := t.ExecuteTemplate(w, inner, data(r)); err != nil {
			slog.DebugContext(r.Context(), "failed to execute template", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func (v *viewer) addMainView() {
	v.addView(main, func(t *template.Template) {
		template.Must(t.ParseFS(viewkit, "main.gohtml"))
		_, _ = t.ParseFS(v.templates, "templates/main-*.gohtml")
	}, func(r *http.Request) any {
		values := r.URL.Query()
		if values.Get(view) == "" {
			values.Add(view, v.cfg.StartView)
		}
		return map[string]any{"Params": values}
	})
}

func (v *viewer) AddView(name, tmpl string, data func(*http.Request) any) {
	v.sources[name] = data
	v.addView(name, func(t *template.Template) { template.Must(t.Parse(tmpl)) }, data)
}

func (v *viewer) AddSource(name string, data func(*http.Request) any) {
	v.sources[name] = data
}

func (v *viewer) addTempView() {
	entries := loader(v.templates.ReadDir, "templates", ".gohtml")

	for _, entry := range entries {
		name := strings.TrimSuffix(path.Base(entry), path.Ext(entry))

		data, ok := v.sources[name]
		if !ok {
			data = func(r *http.Request) any {
				return r.URL.Query()
			}
		}

		v.addView(name, func(t *template.Template) { template.Must(t.ParseFS(v.templates, entry)) }, data)
	}
}

func (v *viewer) Inject(router *http.ServeMux) {
	v.addMainView()
	v.addTempView()

	faviconHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := v.static.Open("static/favicon.ico")
		if err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = f.Close()

		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFileFS(w, r, v.static, "static/favicon.ico")
	})

	router.HandleFunc("/"+v.cfg.Path, v.handler)
	router.Handle("/favicon.ico", faviconHandler)
	router.Handle("/static/", http.FileServer(http.FS(v.static)))
	router.Handle("/viewkit/", http.StripPrefix("/viewkit/", http.FileServer(http.FS(viewkit))))
}
