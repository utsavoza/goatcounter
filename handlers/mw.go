package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sethvargo/go-limiter"
	"zgo.at/errors"
	"zgo.at/goatcounter/v2"
	"zgo.at/goatcounter/v2/pkg/log"
	"zgo.at/guru"
	"zgo.at/json"
	"zgo.at/slog_align"
	"zgo.at/termtext"
	"zgo.at/z18n"
	"zgo.at/zdb"
	"zgo.at/zhttp"
	"zgo.at/zhttp/auth"
	"zgo.at/zhttp/header"
	"zgo.at/zstd/znet"
	"zgo.at/zstd/zruntime"
	"zgo.at/zstd/zslice"
	"zgo.at/zstd/ztime"
	"zgo.at/zvalidate"
)

// Started is set when the server is started.
var Started time.Time

var (
	redirect = func(w http.ResponseWriter, r *http.Request) error {
		zhttp.Flash(w, r, "Need to log in")
		return guru.New(303, goatcounter.Config(r.Context()).BasePath+"/user/new")
	}

	loggedIn = auth.Filter(func(w http.ResponseWriter, r *http.Request) error {
		u := goatcounter.GetUser(r.Context())
		if u != nil && u.ID > 0 {
			err := u.UpdateOpenAt(r.Context())
			if err != nil {
				log.Error(r.Context(), err)
			}
			return nil
		}
		return redirect(w, r)
	})

	loggedInOrPublic = auth.Filter(func(w http.ResponseWriter, r *http.Request) error {
		u := goatcounter.GetUser(r.Context())
		if u != nil && u.ID > 0 {
			err := u.UpdateOpenAt(r.Context())
			if err != nil {
				log.Error(r.Context(), err)
			}
			return nil
		}
		s := Site(r.Context())
		if s.Settings.IsPublic() {
			return nil
		}
		if a := r.URL.Query().Get("access-token"); s.Settings.CanView(a) {
			// Set cookie for auth and redirect. This prevents accidental
			// leaking of the secret by copy/pasting the URL, screenshots, etc.
			http.SetCookie(w, &http.Cookie{
				Name:     "access-token",
				Value:    a,
				Path:     "/",
				HttpOnly: true,
				Secure:   zhttp.IsSecure(r),
				SameSite: zhttp.CookieSameSiteHelper(r),
			})
			hide := ""
			if r.URL.Query().Get("hideui") != "" {
				hide += "?hideui=1"
			}
			return guru.New(303, goatcounter.Config(r.Context()).BasePath+"/"+hide)
		}
		if c, err := r.Cookie("access-token"); err == nil && s.Settings.CanView(c.Value) {
			return nil
		}

		return redirect(w, r)
	})

	requireAccess = func(atLeast goatcounter.UserAccess) func(http.Handler) http.Handler {
		return auth.Filter(func(w http.ResponseWriter, r *http.Request) error {
			u := goatcounter.GetUser(r.Context())
			if u != nil && u.ID > 0 && u.HasAccess(atLeast) {
				return nil
			}
			return guru.Errorf(401, "Not allowed to view this page")
		})
	}

	keyAuth = auth.Add(func(ctx context.Context, key string) (auth.User, error) {
		u := &goatcounter.User{}
		err := u.ByTokenAndSite(ctx, key)
		return u, err
	}, "/bosmang/profile/setrate")
)

type statusWriter interface{ Status() int }

func addctx(db zdb.DB, loadSite bool, dashTimeout int) func(http.Handler) http.Handler {
	Started = time.Now()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			// Intercept /status here so it works everywhere.
			if r.URL.Path == "/status" {
				info, _ := zdb.Info(ctx)
				j, err := json.Marshal(map[string]any{
					"uptime":   ztime.Now(r.Context()).Sub(Started).Round(time.Second).String(),
					"version":  goatcounter.Version,
					"database": zdb.SQLDialect(ctx).String() + " " + string(info.Version),
					"go":       runtime.Version(),
					"GOOS":     runtime.GOOS,
					"GOARCH":   runtime.GOARCH,
					"race":     zruntime.Race,
					"cgo":      zruntime.CGO,
				})
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.WriteHeader(200)

				w.Write(j)
				return
			}

			// Add timeout.
			t := 3
			if r.URL.Path == "/" {
				t = dashTimeout + 1
			}
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(r.Context(), time.Duration(t)*time.Second)
			defer func() {
				cancel()
				if ctx.Err() == context.DeadlineExceeded {
					if ww, ok := w.(statusWriter); !ok || ww.Status() == 0 {
						w.WriteHeader(http.StatusGatewayTimeout)
						w.Write([]byte("Server timed out"))
					}
				}
			}()

			// Wrap in explainDB for testing.
			if goatcounter.Config(r.Context()).Dev {
				if c, _ := r.Cookie("debug-explain"); c != nil {
					*r = *r.WithContext(zdb.WithDB(ctx, zdb.NewLogDB(zdb.MustGetDB(ctx),
						os.Stderr, zdb.DumpQuery|zdb.DumpExplain, c.Value)))
				}
				if c, _ := r.Cookie("debug-dump"); c != nil {
					*r = *r.WithContext(zdb.WithDB(ctx, zdb.NewLogDB(zdb.MustGetDB(ctx),
						os.Stderr, zdb.DumpQuery|zdb.DumpResult, c.Value)))
				}
			}

			// Load site from domain.
			if loadSite {
				var s goatcounter.Site // code
				err := s.ByHost(r.Context(), r.Host)

				// If there's just one site then we can just serve that; most
				// people probably have just one site so it's all grand. Do
				// print a warning in the console though.
				if err != nil && !goatcounter.Config(r.Context()).GoatcounterCom {
					var sites goatcounter.Sites
					err2 := sites.UnscopedList(r.Context())
					if err2 == nil && len(sites) == 1 {
						s = sites[0]
						err = nil

						if r.URL.Path == "/" {
							txt := fmt.Sprintf(""+
								"Accessing the site on domain %q, but the configured domain is %q.\n\n"+
								"This will work fine as long as you only have one site, but you *need* to use the "+
								"configured domain if you add a second site so GoatCounter will know which site to use.",
								znet.RemovePort(r.Host), *s.Cname)
							if _, ok := slog.Default().Handler().(slog_align.AlignedHandler); ok {
								txt = termtext.WordWrap(txt, 70, "")
							} else {
								txt = strings.ReplaceAll(txt, "\n\n", " ")
							}
							log.Warn(r.Context(), txt)
						}
					}
					if err2 == nil && len(sites) == 0 {
						noSites(db, w, r)
						return
					}
				}

				if err != nil {
					if zdb.ErrNoRows(err) {
						err = guru.Errorf(400, "no site at this domain (%q)", r.Host)
					}
					zhttp.ErrPage(w, r, err)
					return
				}

				*r = *r.WithContext(goatcounter.WithSite(r.Context(), &s))
			}

			// Make sure there's always a z18n object; will get overriden by
			// addz18n() later for endpoints where it matters.
			*r = *r.WithContext(z18n.With(r.Context(), goatcounter.DefaultLocale()))

			next.ServeHTTP(w, r)
		})
	}
}

func noSites(db zdb.DB, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		w.Header().Set("Location", goatcounter.Config(r.Context()).BasePath+"/")
		w.WriteHeader(307)
		return
	}

	var (
		tplErr error
		v      = zvalidate.New()
		args   struct {
			Email     string `json:"email"`
			Password  string `json:"password"`
			Password2 string `json:"password2"`
			Cname     string `json:"vhost"`
		}
	)
	if r.Method == "POST" {
		tplErr = zdb.TX(zdb.WithDB(r.Context(), db), func(ctx context.Context) error {
			if _, err := zhttp.Decode(r, &args); err != nil {
				return err
			}

			v.Required("email", args.Email)
			v.Email("email", args.Email)
			v.Required("password", args.Password)
			v.Required("password_verify", args.Password2)
			v.Len("password", args.Password, 8, 0)
			v.Domain("vhost", args.Cname)
			if args.Password != args.Password2 {
				v.Append("password", "passwords don't match")
			}
			if v.HasErrors() {
				return nil
			}

			cn := args.Cname
			if cn == "" {
				cn = "goatcounter.localhost"
			}
			s := goatcounter.Site{Cname: &cn}
			err := s.Insert(ctx)
			if err != nil {
				return err
			}
			ctx = goatcounter.WithSite(ctx, &s)

			u := goatcounter.User{
				Site:          s.ID,
				Email:         args.Email,
				EmailVerified: true,
				Settings:      s.UserDefaults,
				Password:      []byte(args.Password),
				Access:        goatcounter.UserAccesses{"all": goatcounter.AccessSuperuser},
			}
			err = u.Insert(ctx, false)
			if err != nil {
				return err
			}

			err = u.Login(ctx)
			if err != nil {
				return err
			}

			auth.SetCookie(w, r, *u.LoginToken, cookieDomain(&s, r))
			return nil
		})
		if tplErr != nil {
			tplErr = errors.Unwrap(tplErr) // Remove "zdb.TX fn: "
		}
		if tplErr == nil && !v.HasErrors() {
			zhttp.SeeOther(w, "/")
		}
	}

	if r.Method == "GET" {
		args.Cname = znet.RemovePort(r.Host)
		if args.Cname == "localhost" {
			args.Cname = ""
		}
	}

	err := zhttp.Template(w, "serve_newsite.gohtml", struct {
		Globals
		Validate *zvalidate.Validator
		Error    error
		Email    string
		Cname    string
	}{newGlobals(w, r), &v, tplErr, args.Email, args.Cname})
	if err != nil {
		log.Error(r.Context(), err)
	}
}

func addz18n() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var siteLang, userLang string
			if s := goatcounter.GetSite(r.Context()); s != nil {
				siteLang = s.UserDefaults.Language
			}
			if u := goatcounter.GetUser(r.Context()); u != nil {
				userLang = u.Settings.Language
			}
			*r = *r.WithContext(z18n.With(r.Context(), goatcounter.GetBundle(r.Context()).
				Locale(userLang, siteLang, r.Header.Get("Accept-Language"))))
			next.ServeHTTP(w, r)
		})
	}
}

var (
	defaultFrameAncestors = []string{header.CSPSourceNone}
	allFrameAncestors     = []string{header.CSPSourceStar}
)

func addcsp(domainStatic string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ds := []string{header.CSPSourceSelf, "https://gc.zgo.at"}
			if domainStatic != "" {
				ds = append(ds, domainStatic)
			}

			frame := defaultFrameAncestors
			if s := goatcounter.GetSite(r.Context()); s != nil && len(s.Settings.AllowEmbed) > 0 {
				frame = make([]string, 0, len(s.Settings.AllowEmbed))
				for _, d := range s.Settings.AllowEmbed {
					frame = append(frame, d)
				}
			}

			switch {
			case r.URL.Path == "/api2.html":
				// Allow RapiDoc, and allow static.zgo.at because we don't have
				// access to the {{.Static}} variable in here.
				// TODO: can fix that, actually.
				// TODO: maybe don't load from unpkg?
				ds = append(ds, "https://unpkg.com/rapidoc/dist/rapidoc-min.js", "https://static.zgo.at")
			case r.URL.Path == "/api.html":
				ds = append(ds, header.CSPSourceUnsafeInline)
			case strings.HasPrefix(r.URL.Path, "/counter/"):
				frame = allFrameAncestors
			}

			csp := header.CSPArgs{
				header.CSPFrameAncestors: frame,
				header.CSPFrameSrc:       {header.CSPSourceSelf},
				header.CSPDefaultSrc:     {header.CSPSourceNone},
				header.CSPImgSrc:         zslice.AppendCopy(ds, "data:"),
				header.CSPScriptSrc:      ds,
				header.CSPStyleSrc:       zslice.AppendCopy(ds, header.CSPSourceUnsafeInline),
				header.CSPFontSrc:        ds,
				header.CSPFormAction:     {header.CSPSourceSelf},
				header.CSPManifestSrc:    ds,

				// 'self' does not include websockets, and we need to use
				// "wss://domain.com"; this is difficult because of custom
				// domains and such, so just allow all websockets.
				header.CSPConnectSrc: {header.CSPSourceSelf, "wss:"},
			}

			// Make the visitor counter examples work everywhere (e.g.
			// somecode.goatcounter.com).
			if strings.HasSuffix(r.URL.Path, "/help/visitor-counter") {
				csp[header.CSPConnectSrc] = append(csp[header.CSPConnectSrc], "https://goatcounter.goatcounter.com")
				csp[header.CSPImgSrc] = append(csp[header.CSPImgSrc], "https://goatcounter.goatcounter.com")
				csp[header.CSPFrameSrc] = append(csp[header.CSPFrameSrc], "https://goatcounter.goatcounter.com")
			}

			header.SetCSP(w.Header(), csp)
			next.ServeHTTP(w, r)
		})
	}
}

func Ratelimit(withUA bool, getStore func(r *http.Request) (limiter.Store, string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			store, msg := getStore(r)
			key := r.RemoteAddr
			if withUA {
				// Add in the User-Agent for some endpoints to reduce the
				// problem of multiple people in the same building hitting the
				// limit.
				key += r.UserAgent()
			}
			tokens, remaining, reset, ok, err := store.Take(r.Context(), key)
			if err != nil {
				// The memorystore only returns an error if Close() was called.
				// But log just to be sure.
				log.Module("ratelimit").Error(r.Context(), err, "key", key)
				ok = false
			}

			t := time.Unix(0, int64(reset))
			exp := -time.Since(t)
			retryAfter := strconv.FormatFloat(exp.Seconds(), 'f', 0, 64)
			w.Header().Set("X-Rate-Limit-Limit", strconv.FormatUint(tokens, 10))
			w.Header().Set("X-Rate-Limit-Remaining", strconv.FormatUint(remaining, 10))
			w.Header().Set("X-Rate-Limit-Reset", retryAfter)

			if !ok {
				w.Header().Set("Retry-After", retryAfter)
				w.WriteHeader(http.StatusTooManyRequests)

				if msg == "" {
					msg = fmt.Sprintf("rate limited exceeded; try again in %s", exp)
				}
				if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
					fmt.Fprintf(w, `{"error": %q}`, msg)
				} else {
					fmt.Fprintf(w, "%s\n", msg)
				}
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func addCORS() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			o := r.Header.Get("Origin")
			if o != "" {
				w.Header().Add("Access-Control-Allow-Credentials", "true")
				w.Header().Add("Access-Control-Allow-Origin", "*")
				if r.Method == "OPTIONS" {
					w.Header().Add("Access-Control-Allow-Headers", "Authorization, Content-Type")
					w.Header().Add("Access-Control-Allow-Methods", "DELETE, GET, HEAD, OPTIONS, PATCH, POST, PUT")
					w.Header().Add("Allow", "DELETE, GET, HEAD, OPTIONS, PATCH, POST, PUT")
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
