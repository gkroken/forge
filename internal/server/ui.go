package server

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"forge/internal/auth"
	"forge/internal/format"
	"forge/internal/proxy"
	"forge/internal/repo"
)

//go:embed templates static
var uiFS embed.FS

// cssVer is a short content-hash of style.css, computed once at startup.
// Injected into <link> URLs so browsers cache-bust on deploy.
var cssVer string

func init() {
	data, _ := fs.ReadFile(uiFS, "static/style.css")
	h := sha256.Sum256(data)
	cssVer = hex.EncodeToString(h[:4]) // 8 hex chars is plenty
}

// formatIcons maps format names to inline SVG strings (Simple Icons, CC0).
// fill="currentColor" lets the icon inherit the badge text colour.
var formatIcons = map[string]template.HTML{
	"npm": `<svg aria-hidden="true" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg" fill="currentColor" class="fmt-icon"><path d="M1.763 0C.786 0 0 .786 0 1.763v20.474C0 23.214.786 24 1.763 24h20.474c.977 0 1.763-.786 1.763-1.763V1.763C24 .786 23.214 0 22.237 0zM5.13 5.323l13.837.019-.009 13.836h-3.464l.01-10.382h-3.456L12.04 19.17H5.113z"/></svg>`,
	"maven": `<svg aria-hidden="true" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg" fill="currentColor" class="fmt-icon"><path d="M4.237.001c-.312-.013-.665.072-.828.457-.158.374-.283 1.188-.34 2.276l1.223.591c-.02-.737.007-1.43.076-2.066-.026.299-.056.96.006 2.039.019.342.049.725.088 1.15.002.024.002.047.007.069a45.485 45.485 0 0 0 .309 2.412c.057.368.126.752.195 1.16l-.01.01c.014.01.015.018.014.023l.03.16c.03.162.06.328.093.494l.108.553.056.289a61.72 61.72 0 0 0 .457 2.068c.09.382.186.78.287 1.186.098.386.199.783.309 1.193.096.362.199.735.303 1.117.003.018.012.036.015.055a145.826 145.826 0 0 0 .34 1.185l.049.174c.078.261.158.533.242.805a4.2 4.2 0 0 1-.293-.135l-.19-.654c-.02-.077-.042-.148-.062-.225l-.002-.004-.004-.002c-.087-.3-.17-.607-.257-.916-.023-.087-.044-.173-.069-.263l-.314-1.178c-.1-.381-.194-.765-.29-1.154-.094-.39-.185-.78-.277-1.172-.093-.401-.181-.8-.265-1.203-.085-.396-.161-.798-.24-1.193a50.315 50.315 0 0 1-.211-1.17c-.004-.013-.006-.03-.01-.041l.004-.002c-.057-.386-.116-.77-.174-1.15a60.905 60.905 0 0 1-.154-1.204 27.447 27.447 0 0 1-.172-2.41l-1.22-.59c-.004.074-.01.15-.013.23-.012.294-.02.605-.023.93a45.3 45.3 0 0 0 .006 1.157c.009.37.025.755.045 1.148.02.336.042.675.07 1.022l.002.039.006.004c.003.023.007.05.006.076.033.368.064.739.107 1.115a34.493 34.493 0 0 0 .303 2.125c.01.064.024.131.035.195a23.418 23.418 0 0 0 .547 2.32c.07.237.14.464.21.68.063.182.13.365.194.545.155.422.327.832.512 1.232l.006.004a.318.318 0 0 0 .02.05c.225.485.475.95.755 1.395.01.013.02.033.03.047-.455-.183-1.259-.098-1.253-.097.83.288 1.557.64 2.016 1.175-.183.2-.523.352-.953.477.594.064.924-.039 1.045-.092-.31.26-.483.732-.635 1.24.35-.57.696-.949 1.033-1.094.078.258.162.524.244.788A147.532 147.532 0 0 0 5.157 24a.56.56 0 0 0 .43-.312c.13-.282.83-1.775 1.908-3.875.413 1.303.88 2.679 1.386 4.109a.494.494 0 0 0 .076-.465 103.735 103.735 0 0 1-1.308-3.945c.154-.299.316-.612.484-.932.125.04.255.094.389.155.203.186.352.491.482.84a1.515 1.515 0 0 0-.334-1.098c1.335.258 2.547.09 3.287-.81a3.97 3.97 0 0 0 .192-.258c-.325.304-.682.404-1.313.273.996-.281 1.523-.617 2.035-1.22.12-.145.244-.303.371-.48-.943.722-1.927.822-2.9.493l-.045-.018c.914.02 2.203-.474 3.092-1.189.41-.33.796-.73 1.17-1.21.28-.359.55-.76.82-1.216.234-.393.468-.824.7-1.293a2.83 2.83 0 0 1-.74.137l-.144.008c-.048.002-.093 0-.146.002.885-.198 1.5-.74 1.994-1.447-.24.117-.628.262-1.07.297-.058.006-.12.006-.182.006-.013-.002-.028 0-.047-.002.306-.078.574-.178.81-.309a3.363 3.363 0 0 0 .358-.236c.044-.037.088-.07.13-.106.099-.086.193-.18.28-.287.028-.034.056-.063.08-.098.036-.05.073-.098.104-.146a8.388 8.388 0 0 0 .51-.828c.015-.031.032-.057.046-.088.04-.084.08-.16.11-.227.042-.099.074-.179.092-.238a.515.515 0 0 1-.108.051c-.273.112-.727.187-1.086.201-.004 0-.008 0-.013.004h-.067c.72-.214 1.067-.45 1.422-.818a13.883 13.883 0 0 0 1.154-1.428c.264-.37.505-.738.692-1.072a6.5 6.5 0 0 0 .298-.592c.066-.157.122-.305.172-.45-.466.01-.986.011-1.48 0 .495.01 1.015.007 1.484-.005.5-1.485.063-2.262.063-2.262s-.526-1.212-1.4-.851c-.426.175-1.172.73-2.083 1.56l.514 1.45a17.561 17.561 0 0 1 1.703-1.602c-.257.22-.807.726-1.615 1.644-.256.29-.537.624-.844.997-.017.02-.035.038-.047.06a51.435 51.435 0 0 0-1.666 2.187c-.248.34-.498.704-.765 1.088h-.016c.002.02-.004.028-.01.032l-.101.152c-.104.155-.213.31-.318.47l-.352.534c-.061.09-.124.181-.186.277-.184.282-.367.573-.558.873a97.351 97.351 0 0 0-1.428 2.338 96.866 96.866 0 0 0-1.341 2.343c-.012.017-.02.04-.034.057a197.256 197.256 0 0 0-.668 1.223l-.097.181c-.17.318-.346.642-.52.979 0 .004-.005.008-.006.013-.026.048-.05.093-.072.141-.117.222-.218.424-.45.87a1.352 1.352 0 0 0-.233-.182l.345-.65c.047-.089.096-.177.143-.27l.04-.077.546-1.001.13-.233v-.006l-.001-.006c.169-.31.345-.62.52-.94.051-.087.102-.173.153-.265.224-.395.454-.794.684-1.197a91.685 91.685 0 0 1 2.135-3.504c.247-.386.503-.77.754-1.152.092-.138.182-.272.279-.41a72.9 72.9 0 0 1 .48-.701c.007-.012.019-.024.026-.037h.006c.26-.356.517-.713.773-1.065.278-.373.554-.735.83-1.09a31.075 31.075 0 0 1 1.777-2.075l-.515-1.446c-.06.057-.126.116-.192.178a32.37 32.37 0 0 0-.758.729c-.295.294-.597.606-.912.935a46.032 46.032 0 0 0-1.632 1.838l-.03.033.002.008c-.017.02-.033.044-.054.064-.266.323-.538.649-.801.985a39.105 39.105 0 0 0-1.445 1.95c-.043.06-.085.126-.127.186a26.458 26.458 0 0 0-1.403 2.303c-.13.247-.256.485-.37.715-.096.195-.187.395-.278.591-.21.463-.398.93-.566 1.399l.002.006a.36.36 0 0 0-.026.058c-.108.303-.203.608-.29.914-.14.174-.302.325-.483.46a3.505 3.505 0 0 0-.131-.153 5.148 5.148 0 0 0 .824-2.211 6.4 6.4 0 0 0-.016-1.488c-.046-.4-.126-.82-.238-1.274-.097-.393-.217-.81-.363-1.248-.091.185-.22.367-.379.545l-.086.094c-.029.032-.06.06-.092.094.434-.674.486-1.397.358-2.148a2.722 2.722 0 0 1-.49.85c-.033.038-.072.077-.11.116-.01.007-.019.018-.033.028.144-.24.25-.467.318-.698a1.29 1.29 0 0 0 .04-.146 2.85 2.85 0 0 0 .038-.225l.018-.146a2.11 2.11 0 0 0-.002-.354c-.003-.04-.004-.076-.01-.113-.01-.055-.016-.105-.027-.154a7.416 7.416 0 0 0-.193-.84c-.01-.028-.015-.056-.026-.084-.027-.079-.048-.149-.072-.209a2.1 2.1 0 0 0-.09-.209.455.455 0 0 1-.035.1c-.102.24-.34.57-.557.8-.003.003-.007.005-.007.01l-.04.043c.318-.58.39-.946.385-1.398a12.274 12.274 0 0 0-.16-1.615 10.68 10.68 0 0 0-.232-1.104 5.853 5.853 0 0 0-.18-.558 6.337 6.337 0 0 0-.172-.391 26.18 26.18 0 0 0 .002-.004C5.576.341 4.82.124 4.82.124s-.27-.11-.582-.123zm3.38 15.783l.032.082v.002c-.06.033-.116.067-.178.097-.012.004-.024.012-.039.018a2.41 2.41 0 0 0 .186-.2zm-.603 1.626c.13.136.25.242.354.32l.07.227a1.866 1.866 0 0 0-.246.053l-.03-.098c-.024-.084-.048-.17-.076-.257l-.021-.073zm.26.875a2.34 2.34 0 0 1 .271.01l.07.229a.778.778 0 0 1 .247-.004l-.326.627a127.643 127.643 0 0 1-.262-.862z"/></svg>`,
	"helm": `<svg aria-hidden="true" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg" fill="currentColor" class="fmt-icon"><path d="M12.337 0c-.475 0-.861 1.016-.861 2.269 0 .527.069 1.011.183 1.396a8.514 8.514 0 0 0-3.961 1.22 5.229 5.229 0 0 0-.595-1.093c-.606-.866-1.34-1.436-1.79-1.43a.381.381 0 0 0-.217.066c-.39.273-.123 1.326.596 2.353.267.381.559.705.84.948a8.683 8.683 0 0 0-1.528 1.716h1.734a7.179 7.179 0 0 1 5.381-2.421 7.18 7.18 0 0 1 5.382 2.42h1.733a8.687 8.687 0 0 0-1.32-1.53c.35-.249.735-.643 1.078-1.133.719-1.027.986-2.08.596-2.353a.382.382 0 0 0-.217-.065c-.45-.007-1.184.563-1.79 1.43a4.897 4.897 0 0 0-.676 1.325a8.52 8.52 0 0 0-3.899-1.42c.12-.39.193-.887.193-1.429 0-1.253-.386-2.269-.862-2.269zM1.624 9.443v5.162h1.358v-1.968h1.64v1.968h1.357V9.443H4.62v1.838H2.98V9.443zm5.912 0v5.162h3.21v-1.108H8.893v-.95h1.64v-1.142h-1.64v-.84h1.853V9.443zm4.698 0v5.162h3.218v-1.362h-1.86v-3.8zm4.706 0v5.162h1.364v-2.643l1.357 1.225 1.35-1.232v2.65h1.365V9.443h-.614l-2.1 1.914-2.109-1.914zm-11.82 7.28a8.688 8.688 0 0 0 1.412 1.548 5.206 5.206 0 0 0-.841.948c-.719 1.027-.985 2.08-.596 2.353.39.273 1.289-.338 2.007-1.364a5.23 5.23 0 0 0 .595-1.092a8.514 8.514 0 0 0 3.961 1.219 5.01 5.01 0 0 0-.183 1.396c0 1.253.386 2.269.861 2.269.476 0 .862-1.016.862-2.269 0-.542-.072-1.04-.193-1.43a8.52 8.52 0 0 0 3.9-1.42c.121.4.352.865.675 1.327.719 1.026 1.617 1.637 2.007 1.364.39-.273.123-1.326-.596-2.353-.343-.49-.727-.885-1.077-1.135a8.69 8.69 0 0 0 1.202-1.36h-1.771a7.174 7.174 0 0 1-5.227 2.252 7.174 7.174 0 0 1-5.226-2.252z"/></svg>`,
	"cran": `<svg aria-hidden="true" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg" fill="currentColor" class="fmt-icon"><path d="M12 2.746c-6.627 0-12 3.599-12 8.037 0 3.897 4.144 7.144 9.64 7.88V16.26c-2.924-.915-4.925-2.755-4.925-4.877 0-3.035 4.084-5.494 9.12-5.494 5.038 0 8.757 1.683 8.757 5.494 0 1.976-.999 3.379-2.662 4.272.09.066.174.128.258.216.169.149.25.363.372.544 2.128-1.45 3.44-3.437 3.44-5.631 0-4.44-5.373-8.038-12-8.038zm-2.111 4.99v13.516l4.093-.002-.002-5.291h1.1c.225 0 .321.066.549.25.272.22.715.982.715.982l2.164 4.063 4.627-.002-2.864-4.826s-.086-.193-.265-.383a2.22 2.22 0 00-.582-.416c-.422-.214-1.149-.434-1.149-.434s3.578-.264 3.578-3.826c0-3.562-3.744-3.63-3.744-3.63zm4.127 2.93l2.478.002s1.149-.062 1.149 1.127c0 1.165-1.149 1.17-1.149 1.17h-2.478zm1.754 6.119c-.494.049-1.012.079-1.54.088v1.807a16.622 16.622 0 002.37-.473l-.471-.891s-.108-.183-.248-.394c-.039-.054-.08-.098-.111-.137z"/></svg>`,
	"oci":  `<svg aria-hidden="true" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg" fill="currentColor" class="fmt-icon"><path d="M13.983 11.078h2.119a.186.186 0 00.186-.185V9.006a.186.186 0 00-.186-.186h-2.119a.185.185 0 00-.185.185v1.888c0 .102.083.185.185.185m-2.954-5.43h2.118a.186.186 0 00.186-.186V3.574a.186.186 0 00-.186-.185h-2.118a.185.185 0 00-.185.185v1.888c0 .102.082.185.185.185m0 2.716h2.118a.187.187 0 00.186-.186V6.29a.186.186 0 00-.186-.185h-2.118a.185.185 0 00-.185.185v1.887c0 .102.082.185.185.186m-2.93 0h2.12a.186.186 0 00.184-.186V6.29a.185.185 0 00-.185-.185H8.1a.185.185 0 00-.185.185v1.887c0 .102.083.185.185.186m-2.964 0h2.119a.186.186 0 00.185-.186V6.29a.185.185 0 00-.185-.185H5.136a.186.186 0 00-.186.185v1.887c0 .102.084.185.186.186m5.893 2.715h2.118a.186.186 0 00.186-.185V9.006a.186.186 0 00-.186-.186h-2.118a.185.185 0 00-.185.185v1.888c0 .102.082.185.185.185m-2.93 0h2.12a.185.185 0 00.184-.185V9.006a.185.185 0 00-.184-.186h-2.12a.185.185 0 00-.184.185v1.888c0 .102.083.185.185.185m-2.964 0h2.119a.185.185 0 00.185-.185V9.006a.185.185 0 00-.184-.186h-2.12a.186.186 0 00-.186.186v1.887c0 .102.084.185.186.185m-2.92 0h2.12a.185.185 0 00.184-.185V9.006a.185.185 0 00-.184-.186h-2.12a.185.185 0 00-.184.185v1.888c0 .102.082.185.185.185M23.763 9.89c-.065-.051-.672-.51-1.954-.51-.338.001-.676.03-1.01.087-.248-1.7-1.653-2.53-1.716-2.566l-.344-.199-.226.327c-.284.438-.49.922-.612 1.43-.23.97-.09 1.882.403 2.661-.595.332-1.55.413-1.744.42H.751a.751.751 0 00-.75.748 11.376 11.376 0 00.692 4.062c.545 1.428 1.355 2.48 2.41 3.124 1.18.723 3.1 1.137 5.275 1.137.983.003 1.963-.086 2.93-.266a12.248 12.248 0 003.823-1.389c.98-.567 1.86-1.288 2.61-2.136 1.252-1.418 1.998-2.997 2.553-4.4h.221c1.372 0 2.215-.549 2.68-1.009.309-.293.55-.65.707-1.046l.098-.288Z"/></svg>`,
}

func formatIcon(format string) template.HTML {
	return formatIcons[format]
}

var uiFuncs = template.FuncMap{
	"join":        strings.Join,
	"formatIcon":  formatIcon,
	"add":  func(a, b int) int { return a + b },
	"sub":  func(a, b int) int { return a - b },
	"slice3": func(ss []string) []string {
		if len(ss) <= 3 {
			return ss
		}
		return ss[:3]
	},
	"durStr": func(d time.Duration) string {
		if d == 0 {
			return ""
		}
		return d.String()
	},
	"fmtTime": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02")
	},
	"deref": func(b *bool) bool {
		if b == nil {
			return false
		}
		return *b
	},
	"urlPathEscape": url.PathEscape,
	"cssVer":        func() string { return cssVer },
	// sortURL builds the href for a column sort link.
	// Clicking the active column flips the direction; a new column sorts asc.
	"sortURL": func(col, activeCol, activeDir string) string {
		dir := "asc"
		if col == activeCol && activeDir == "asc" {
			dir = "desc"
		}
		return "/ui/?sort=" + col + "&dir=" + dir
	},
	// sortIcon returns ▲/▼ for the active column, empty for inactive columns.
	"sortIcon": func(col, activeCol, activeDir string) string {
		if col != activeCol {
			return ""
		}
		if activeDir == "desc" {
			return " ▼"
		}
		return " ▲"
	},
}

func parseUITmpl(files ...string) *template.Template {
	return template.Must(template.New("").Funcs(uiFuncs).ParseFS(uiFS, files...))
}

var (
	tmplHome             = parseUITmpl("templates/base.html", "templates/home.html")
	tmplRepo             = parseUITmpl("templates/base.html", "templates/repo.html")
	tmplSearch           = parseUITmpl("templates/base.html", "templates/search.html")
	tmplAdminRepos       = parseUITmpl("templates/base.html", "templates/admin_repos.html")
	tmplAdminForm        = parseUITmpl("templates/base.html", "templates/admin_repo_form.html")
	tmplLogin            = parseUITmpl("templates/base.html", "templates/login.html")
	tmplComponent        = parseUITmpl("templates/base.html", "templates/component.html")
	tmplTokens           = parseUITmpl("templates/base.html", "templates/tokens.html")
	tmplAccess           = parseUITmpl("templates/base.html", "templates/access.html")
	tmplUpload           = parseUITmpl("templates/base.html", "templates/upload.html")
	// Foundry admin shell — sidebar layout
	tmplDashboard        = parseUITmpl("templates/admin_shell.html", "templates/dashboard.html")
	tmplAdminTokens      = parseUITmpl("templates/admin_shell.html", "templates/tokens_admin.html")
	tmplCleanupPolicies  = parseUITmpl("templates/admin_shell.html", "templates/cleanup_policies.html")
	tmplObservability    = parseUITmpl("templates/admin_shell.html", "templates/observability.html")
)

// ── page data types ───────────────────────────────────────────────────────────

type componentPage struct {
	Title  string
	Repo   repo.Repository
	Detail format.ComponentDetail
}

type loginPage struct {
	Title       string
	Error       string
	Next        string
	OIDCEnabled bool
}

type homePage struct {
	Title string
	Repos []repoRow
	Sort  string // active sort column: "name"|"format"|"kind"|"count"
	Dir   string // "asc"|"desc"
}

type repoRow struct {
	Name       string
	Format     string
	Kind       string
	Count      int  // -1 = browse not supported for this format
	UpstreamOK *bool // nil = no data yet; true = healthy; false = error
}

type repoPage struct {
	Title      string
	Repo       repo.Repository
	Components []componentItem
	Total      int
	Page       int
	Limit      int
	HasMore    bool
	Query      string
}

type searchPage struct {
	Title      string
	Query      string
	Format     string
	Repo       string
	AllFormats []string
	AllRepos   []string
	Results    []searchResult
}

// ── dispatcher ────────────────────────────────────────────────────────────────

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/ui")
	p = strings.TrimRight(p, "/")
	if p == "" {
		p = "/"
	}
	switch {
	case p == "/":
		s.uiHome(w, r)
	case p == "/dashboard":
		s.uiDashboard(w, r)
	case p == "/search":
		s.uiSearch(w, r)
	case p == "/login":
		s.uiLogin(w, r)
	case p == "/logout":
		s.uiLogout(w, r)
	case strings.HasPrefix(p, "/repos/"):
		rest := strings.TrimPrefix(p, "/repos/")
		if rest == "" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		// /repos/{name} → repo detail
		// /repos/{name}/upload → upload page
		// /repos/{name}/{component} → component detail (strings.Cut on first "/" preserves @scope/pkg)
		repoName, sub, hasComponent := strings.Cut(rest, "/")
		if hasComponent && sub == "upload" {
			s.uiUpload(w, r, repoName)
		} else if hasComponent && sub != "" {
			s.uiComponent(w, r, repoName, sub)
		} else {
			s.uiRepo(w, r, repoName)
		}
	case p == "/admin" || strings.HasPrefix(p, "/admin/"):
		s.handleUIAdmin(w, r, strings.TrimPrefix(p, "/admin"))
	default:
		http.NotFound(w, r)
	}
}

// ── handlers ──────────────────────────────────────────────────────────────────

func (s *Server) uiHome(w http.ResponseWriter, r *http.Request) {
	var rows []repoRow
	for _, rp := range s.Repos.All() {
		count := -1
		if h, ok := s.Handlers.For(rp.Format); ok {
			if b, ok := h.(format.Browsable); ok {
				c := s.browseCtx(rp)
				if entries, err := b.BrowseRepo(c); err == nil {
					count = len(entries)
				}
			}
		}
		row := repoRow{
			Name: rp.Name, Format: rp.Format,
			Kind: string(rp.Kind), Count: count,
		}
		if rp.Kind == repo.Proxy {
			var hr proxy.HealthRecord
			c := s.browseCtx(rp)
			if ok, _ := c.Meta.GetJSON(rp.Name+":proxy", proxy.HealthKey, &hr); ok {
				row.UpstreamOK = &hr.OK
			}
		}
		rows = append(rows, row)
	}

	sortCol := r.URL.Query().Get("sort")
	sortDir := r.URL.Query().Get("dir")
	if sortDir != "desc" {
		sortDir = "asc"
	}
	if sortCol != "" {
		sort.SliceStable(rows, func(i, j int) bool {
			var less bool
			switch sortCol {
			case "format":
				less = rows[i].Format < rows[j].Format
			case "kind":
				less = rows[i].Kind < rows[j].Kind
			case "count":
				ci, cj := rows[i].Count, rows[j].Count
				if ci < 0 {
					ci = -1
				}
				if cj < 0 {
					cj = -1
				}
				less = ci < cj
			default: // "name"
				sortCol = "name"
				less = rows[i].Name < rows[j].Name
			}
			if sortDir == "desc" {
				return !less
			}
			return less
		})
	}

	render(w, tmplHome, "base.html", homePage{
		Title: "Repositories", Repos: rows,
		Sort: sortCol, Dir: sortDir,
	})
}

func (s *Server) uiRepo(w http.ResponseWriter, r *http.Request, name string) {
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.NotFound(w, r)
		return
	}

	q := r.URL.Query().Get("q")
	page := clampedInt(r, "page", 1, 1, 1<<20)
	const limit = 50

	var components []componentItem
	var total int

	if h, ok := s.Handlers.For(rp.Format); ok {
		if b, ok := h.(format.Browsable); ok {
			if entries, err := b.BrowseRepo(s.browseCtx(rp)); err == nil {
				if q != "" {
					ql := strings.ToLower(q)
					kept := entries[:0]
					for _, e := range entries {
						if strings.Contains(strings.ToLower(e.Name), ql) {
							kept = append(kept, e)
						}
					}
					entries = kept
				}
				total = len(entries)
				start := (page - 1) * limit
				if start < total {
					end := start + limit
					if end > total {
						end = total
					}
					for _, e := range entries[start:end] {
						components = append(components, componentItem{
							Name: e.Name, Versions: e.Versions, UpdatedAt: e.UpdatedAt,
						})
					}
				}
			}
		}
	}

	data := repoPage{
		Title: rp.Name, Repo: rp,
		Components: components, Total: total,
		Page: page, Limit: limit,
		HasMore: page*limit < total,
		Query:   q,
	}
	if r.Header.Get("HX-Request") == "true" {
		render(w, tmplRepo, "components-section", data)
		return
	}
	render(w, tmplRepo, "base.html", data)
}

func (s *Server) uiComponent(w http.ResponseWriter, r *http.Request, repoName, component string) {
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		http.NotFound(w, r)
		return
	}
	c := s.browseCtx(rp)
	base := publicBase(r)

	// Inspectable provides rich detail; fall back to BrowseRepo for versions only.
	if insp, ok := h.(format.Inspectable); ok {
		detail, found := insp.Inspect(c, base, component)
		if !found {
			http.NotFound(w, r)
			return
		}
		render(w, tmplComponent, "base.html", componentPage{
			Title:  component + " — " + repoName,
			Repo:   rp,
			Detail: detail,
		})
		return
	}

	b, ok := h.(format.Browsable)
	if !ok {
		http.NotFound(w, r)
		return
	}
	entries, err := b.BrowseRepo(c)
	if err != nil {
		http.Error(w, "browse error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var detail format.ComponentDetail
	for _, e := range entries {
		if e.Name == component {
			detail.Name = e.Name
			detail.Versions = make([]format.VersionInfo, len(e.Versions))
			for i, v := range e.Versions {
				detail.Versions[i] = format.VersionInfo{Version: v}
			}
			break
		}
	}
	if detail.Name == "" {
		http.NotFound(w, r)
		return
	}
	detail.InstallSnippet = base + "/repository/" + repoName + "/"
	render(w, tmplComponent, "base.html", componentPage{
		Title:  component + " — " + repoName,
		Repo:   rp,
		Detail: detail,
	})
}

func (s *Server) uiSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	filterFormat := r.URL.Query().Get("format")
	filterRepo := r.URL.Query().Get("repo")

	var results []searchResult
	if ql := strings.ToLower(strings.TrimSpace(q)); ql != "" {
		for _, rp := range s.Repos.All() {
			if filterFormat != "" && rp.Format != filterFormat {
				continue
			}
			if filterRepo != "" && rp.Name != filterRepo {
				continue
			}
			h, ok := s.Handlers.For(rp.Format)
			if !ok {
				continue
			}
			b, ok := h.(format.Browsable)
			if !ok {
				continue
			}
			entries, err := b.BrowseRepo(s.browseCtx(rp))
			if err != nil {
				continue
			}
			for _, e := range entries {
				if strings.Contains(strings.ToLower(e.Name), ql) {
					results = append(results, searchResult{
						Repo: rp.Name, Format: rp.Format,
						Name: e.Name, Versions: e.Versions,
					})
				}
			}
		}
	}
	if results == nil {
		results = []searchResult{}
	}

	// Build repo list for the dropdown (all repos that support browsing).
	var allRepos []string
	for _, rp := range s.Repos.All() {
		if _, ok := s.Handlers.For(rp.Format); ok {
			allRepos = append(allRepos, rp.Name)
		}
	}

	data := searchPage{
		Title:      "Search",
		Query:      q,
		Format:     filterFormat,
		Repo:       filterRepo,
		AllFormats: allFormats,
		AllRepos:   allRepos,
		Results:    results,
	}
	// Boosted nav-bar requests (hx-boost) want the full page; only return the
	// partial fragment for direct htmx swap calls from the search page itself.
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" {
		render(w, tmplSearch, "search-results", data)
		return
	}
	render(w, tmplSearch, "base.html", data)
}

func (s *Server) uiLogin(w http.ResponseWriter, r *http.Request) {
	next := sanitizeNext(r.URL.Query().Get("next"))

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		secret := r.FormValue("token")
		if n := r.FormValue("next"); n != "" {
			next = sanitizeNext(n)
		}

		ok := s.verifyAdminSecret(secret)
		if !ok {
			render(w, tmplLogin, "base.html", loginPage{
				Title: "Sign in",
				Error: "Invalid token or insufficient permissions.",
				Next:  next,
			})
			return
		}
		http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure set via isSecureContext; HttpOnly+SameSiteStrict already present
			Name:     auth.UISessionCookie,
			Value:    secret,
			Path:     "/",
			HttpOnly: true,
			Secure:   isSecureContext(r),
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, next, http.StatusSeeOther) // #nosec G710 -- next is always output of sanitizeNext(), which rejects absolute URLs
		return
	}

	errMsg := ""
	switch r.URL.Query().Get("error") {
	case "invalid":
		errMsg = "Invalid token or insufficient permissions."
	case "oidc":
		errMsg = "SSO login failed. Please try again or sign in with a token."
	}
	render(w, tmplLogin, "base.html", loginPage{
		Title:       "Sign in",
		Error:       errMsg,
		Next:        next,
		OIDCEnabled: s.OIDC != nil && s.Auth != nil,
	})
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure set via isSecureContext; HttpOnly+SameSiteStrict already present
		Name:     auth.UISessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isSecureContext(r),
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// verifyAdminSecret returns true if secret is a valid admin token (or if auth
// is not enabled in eval mode).
func (s *Server) verifyAdminSecret(secret string) bool {
	if s.Auth == nil {
		return true
	}
	tok, err := s.Auth.Verify(secret)
	return err == nil && tok != nil && tok.RoleFor("*") >= auth.RoleAdmin
}

// sanitizeNext ensures the redirect target is a safe forge UI path,
// preventing open redirects to external URLs. It parses the URL, rejects
// absolute URLs, and strips any query/fragment so only the path is returned.
func sanitizeNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || !strings.HasPrefix(u.Path, "/ui/") {
		return "/ui/admin/"
	}
	return u.Path
}

// isSecureContext reports whether the request arrived over TLS, either
// directly or via a reverse proxy that sets X-Forwarded-Proto.
func isSecureContext(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// ── helpers ───────────────────────────────────────────────────────────────────

// publicBase returns the scheme+host for the current request, respecting
// X-Forwarded-Proto set by reverse proxies.
func publicBase(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto + "://" + r.Host
	}
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

// browseCtx builds a format.Context suitable for BrowseRepo calls (no Sub/Queue).
func (s *Server) browseCtx(rp repo.Repository) *format.Context {
	return &format.Context{
		Repo: rp, Blob: s.Blob, Meta: s.Meta,
		HTTP: s.client, Repos: s.Repos, Metrics: s.Metrics,
	}
}

// render executes a named template into a buffer, then writes it to w.
// Buffering ensures a clean 500 if template rendering fails mid-output.
func render(w http.ResponseWriter, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w) //nolint:errcheck
}

// serveUIStatic returns a handler for /ui/static/ backed by the embedded FS.
func (s *Server) serveUIStatic() http.Handler {
	sub, _ := fs.Sub(uiFS, "static")
	return http.StripPrefix("/ui/static/", http.FileServer(http.FS(sub)))
}
