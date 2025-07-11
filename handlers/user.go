package handlers

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sethvargo/go-limiter"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/xsrftoken"
	"zgo.at/blackmail"
	"zgo.at/goatcounter/v2"
	"zgo.at/goatcounter/v2/pkg/bgrun"
	"zgo.at/goatcounter/v2/pkg/log"
	"zgo.at/guru"
	"zgo.at/otp"
	"zgo.at/zdb"
	"zgo.at/zhttp"
	"zgo.at/zhttp/auth"
	"zgo.at/zvalidate"
)

const (
	actionTOTP = "totp"
	mfaError   = "Token did not match; perhaps you waited too long? Try again."
)

var testTOTP = false

type user struct{}

func (h user) mount(r chi.Router, ratelimits Ratelimits) {
	r.Get("/user/new", zhttp.Wrap(h.login))
	r.Get("/user/forgot", zhttp.Wrap(h.forgot))
	r.Post("/user/request-reset", zhttp.Wrap(h.requestReset))

	// Rate limit login attempts.
	rate := r.With(Ratelimit(false, func(*http.Request) (limiter.Store, string) {
		return ratelimits.Login, ""
	}))
	rate.Post("/user/requestlogin", zhttp.Wrap(h.requestLogin))
	r.Get("/user/requestlogin", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redirect, as panic()s and such can end up here.
		zhttp.SeeOther(w, "/user/new")
	}))
	rate.Post("/user/totplogin", zhttp.Wrap(h.totpLogin))
	rate.Get("/user/reset/{key}", zhttp.Wrap(h.reset))
	rate.Get("/user/verify/{key}", zhttp.Wrap(h.verify))
	rate.Post("/user/reset/{key}", zhttp.Wrap(h.doReset))

	auth := r.With(loggedIn, addz18n())
	auth.Post("/user/logout", zhttp.Wrap(h.logout))
	auth.Post("/user/change-password", zhttp.Wrap(h.changePassword))
	auth.Post("/user/disable-totp", zhttp.Wrap(h.disableTOTP))
	auth.Post("/user/enable-totp", zhttp.Wrap(h.enableTOTP))
	auth.Post("/user/resend-verify", zhttp.Wrap(h.resendVerify))
}

func (h user) login(w http.ResponseWriter, r *http.Request) error {
	u := User(r.Context())
	if u != nil && u.ID > 0 {
		return zhttp.SeeOther(w, "/")
	}

	return zhttp.Template(w, "user.gohtml", struct {
		Globals
		Email string
	}{newGlobals(w, r), r.URL.Query().Get("email")})
}

func (h user) forgot(w http.ResponseWriter, r *http.Request) error {
	u := User(r.Context())
	if u != nil && u.ID > 0 {
		return zhttp.SeeOther(w, "/")
	}

	return zhttp.Template(w, "user_forgot_pw.gohtml", struct {
		Globals
		Email    string
		Page     string
		MetaDesc string
	}{newGlobals(w, r), r.URL.Query().Get("email"), "forgot_pw", "Forgot password – GoatCounter"})
}

func (h user) requestReset(w http.ResponseWriter, r *http.Request) error {
	u := User(r.Context())
	if u != nil && u.ID > 0 {
		return zhttp.SeeOther(w, "/")
	}

	// Legacy email flow.
	args := struct {
		Email string `json:"email"`
	}{}
	_, err := zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	err = u.ByEmail(r.Context(), args.Email)
	if err != nil {
		if zdb.ErrNoRows(err) {
			zhttp.FlashError(w, r, T(r.Context(), "error/reset-user-no-account|Not an account on this site: %(email)", args.Email))
			return zhttp.SeeOther(w, fmt.Sprintf("/user/new?email=%s", url.QueryEscape(args.Email)))
		}
		return err
	}

	err = u.RequestReset(r.Context())
	if err != nil {
		return err
	}

	site := Site(r.Context())
	ctx := goatcounter.CopyContextValues(r.Context())
	bgrun.RunFunction("email:password", func() {
		err := blackmail.Send(
			T(ctx, "email/reset-user-email-subject|Password reset for %(domain)", site.Domain(ctx)),
			blackmail.From("GoatCounter login", goatcounter.Config(ctx).EmailFrom),
			blackmail.To(u.Email),
			blackmail.HeadersAutoreply(),
			blackmail.BodyMustText(goatcounter.TplEmailPasswordReset{ctx, *site, *u}.Render))
		if err != nil {
			log.Errorf(ctx, "password reset: %s", err)
		}
	})

	zhttp.Flash(w, r, T(r.Context(), "notify/reset-user-sent|Email sent to %(email)", args.Email))
	return zhttp.SeeOther(w, "/user/forgot")
}

func (h user) requestLogin(w http.ResponseWriter, r *http.Request) error {
	u := User(r.Context())
	if u != nil && u.ID > 0 { // Already logged in.
		return zhttp.SeeOther(w, "/")
	}

	args := struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}{}
	_, err := zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	var user goatcounter.User
	err = user.ByEmail(r.Context(), args.Email)
	if err != nil {
		if zdb.ErrNoRows(err) {
			zhttp.FlashError(w, r, T(r.Context(), "error/login-not-found|User %(email) not found", args.Email))
			return zhttp.SeeOther(w, "/user/new")
		}
		return err
	}

	if len(user.Password) == 0 {
		zhttp.FlashError(w, r, T(r.Context(), "error/login-no-password|There is no password set for %(email); please reset it", args.Email))
		return zhttp.SeeOther(w, "/user/forgot?email="+url.QueryEscape(args.Email))
	}

	err = bcrypt.CompareHashAndPassword(user.Password, []byte(args.Password))
	if err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			zhttp.FlashError(w, r, T(r.Context(), "error/login-wrong-pwd|Wrong password for %(email)", args.Email))
		} else {
			zhttp.FlashError(w, r, "Something went wrong :-( An error has been logged for investigation.") // TODO: should be more generic
			log.Error(r.Context(), err, log.AttrHTTP(r))
		}
		return zhttp.SeeOther(w, "/user/new?email="+url.QueryEscape(args.Email))
	}

	err = user.Login(r.Context())
	if err != nil {
		return err
	}

	if user.TOTPEnabled {
		return h.totpForm(w, r, *user.LoginToken,
			xsrftoken.Generate(*user.LoginToken, strconv.Itoa(int(user.ID)), actionTOTP))
	}

	auth.SetCookie(w, r, *user.LoginToken, cookieDomain(Site(r.Context()), r))
	return zhttp.SeeOther(w, "/")
}

func (h user) totpLogin(w http.ResponseWriter, r *http.Request) error {
	args := struct {
		LoginMAC       string `json:"loginmac"`
		UserLoginToken string `json:"user_logintoken"`
		Token          string `json:"totp_token"`
	}{}
	_, err := zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	var u goatcounter.User
	err = u.ByTokenAndSite(r.Context(), args.UserLoginToken)
	if err != nil {
		return err
	}

	valid := xsrftoken.Valid(args.LoginMAC, *u.LoginToken, strconv.Itoa(int(u.ID)), actionTOTP)
	if testTOTP {
		valid = true
	}
	if !valid {
		zhttp.Flash(w, r, T(r.Context(), "error/login-invalid|Invalid login"))
		return zhttp.SeeOther(w, "/user/new")
	}

	if !testTOTP {
		o := otp.New(u.TOTPSecret, 6, sha1.New, otp.TOTP(30*time.Second, time.Now))
		if !o.Verify(args.Token, 1) {
			zhttp.FlashError(w, r, mfaError)
			return h.totpForm(w, r, *u.LoginToken, args.LoginMAC)
		}
	}

	auth.SetCookie(w, r, *u.LoginToken, cookieDomain(Site(r.Context()), r))
	return zhttp.SeeOther(w, "/")
}

func (h user) totpForm(w http.ResponseWriter, r *http.Request, loginToken, loginMAC string) error {
	return zhttp.Template(w, "totp.gohtml", struct {
		Globals
		LoginToken string
		LoginMAC   string
	}{newGlobals(w, r), loginToken, loginMAC})
}

func (h user) reset(w http.ResponseWriter, r *http.Request) error {
	site := Site(r.Context())
	key := chi.URLParam(r, "key")

	var user goatcounter.User
	err := user.ByResetToken(r.Context(), key)
	if err != nil {
		if !zdb.ErrNoRows(err) {
			log.Error(r.Context(), err)
		}
		return guru.New(http.StatusForbidden, T(r.Context(),
			"error/login-token-expired|Could not find the user for the given token; perhaps it's expired or has already been used?"))
	}

	user.ID = 0 // Don't count as logged in.

	return zhttp.Template(w, "user_reset.gohtml", struct {
		Globals
		Site *goatcounter.Site
		User goatcounter.User
		Key  string
	}{newGlobals(w, r), site, user, key})
}

func (h user) doReset(w http.ResponseWriter, r *http.Request) error {
	var user goatcounter.User
	err := user.ByResetToken(r.Context(), chi.URLParam(r, "key"))
	if err != nil {
		return guru.New(http.StatusForbidden,
			T(r.Context(), "notify/no-user-for-token|could find the user for the given token; perhaps it's expired or has already been used?"))
	}

	var args struct {
		Password  string `json:"password"`
		Password2 string `json:"password2"`
	}
	_, err = zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	if args.Password != args.Password2 {
		zhttp.FlashError(w, r, T(r.Context(), "error/password-does-not-match|Password confirmation doesn’t match."))
		return zhttp.SeeOther(w, "/user/new")
	}

	err = zdb.TX(r.Context(), func(ctx context.Context) error {
		err = user.UpdatePassword(ctx, args.Password)
		if err != nil {
			return err
		}

		// Might as well verify the email here, as you can only get the token
		// from email.
		if !user.EmailVerified {
			return user.VerifyEmail(ctx)
		}
		return nil
	})
	if err != nil {
		var vErr *zvalidate.Validator
		if errors.As(err, &vErr) {
			zhttp.FlashError(w, r, fmt.Sprintf("%s", err))
			return zhttp.SeeOther(w, "/user/new")
		}
		return err
	}

	zhttp.Flash(w, r, T(r.Context(), "notify/login-after-password-reset|Password reset; use your new password to login."))
	return zhttp.SeeOther(w, "/user/new")
}

func (h user) logout(w http.ResponseWriter, r *http.Request) error {
	if goatcounter.Config(r.Context()).GoatcounterCom {
		isBosmang := false
		for _, c := range r.Cookies() {
			if c.Name == "is_bosmang" {
				isBosmang = true
				break
			}
		}
		if isBosmang {
			auth.ClearCookie(w, Site(r.Context()).Domain(r.Context()))
			return zhttp.SeeOther(w, "https://www.goatcounter.com")
		}
	}

	u := User(r.Context())
	err := u.Logout(r.Context())
	if err != nil {
		log.Errorf(r.Context(), "logout: %s", err)
	}

	auth.ClearCookie(w, Site(r.Context()).Domain(r.Context()))
	return zhttp.SeeOther(w, "/")
}

func (h user) disableTOTP(w http.ResponseWriter, r *http.Request) error {
	u := User(r.Context())
	err := u.DisableTOTP(r.Context())
	if err != nil {
		return err
	}

	zhttp.Flash(w, r, T(r.Context(), "notify/disabled-multi-factor-auth|Multi-factor authentication disabled."))
	return zhttp.SeeOther(w, "/user/auth")
}

func (h user) enableTOTP(w http.ResponseWriter, r *http.Request) error {
	u := User(r.Context())
	var args struct {
		Token string `json:"totp_token"`
	}
	_, err := zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	o := otp.New(u.TOTPSecret, 6, sha1.New, otp.TOTP(30*time.Second, time.Now))
	if !o.Verify(args.Token, 1) {
		zhttp.FlashError(w, r, mfaError)
		return zhttp.SeeOther(w, "/user/auth")
	}

	err = u.EnableTOTP(r.Context())
	if err != nil {
		return err
	}

	zhttp.Flash(w, r, T(r.Context(), "notify/multi-factor-auth-enabled|Multi-factor authentication enabled."))
	return zhttp.SeeOther(w, "/user/auth")
}

func (h user) changePassword(w http.ResponseWriter, r *http.Request) error {
	u := User(r.Context())
	var args struct {
		CPassword string `json:"c_password"`
		Password  string `json:"password"`
		Password2 string `json:"password2"`
	}
	_, err := zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	if len(u.Password) > 0 {
		ok, err := u.CorrectPassword(args.CPassword)
		if err != nil {
			return err
		}
		if !ok {
			zhttp.FlashError(w, r, T(r.Context(), "error/incorrect-password|Current password is incorrect."))
			return zhttp.SeeOther(w, "/user/auth")
		}
	}

	if args.Password != args.Password2 {
		zhttp.FlashError(w, r, T(r.Context(), "error/password-does-not-match|Password confirmation doesn’t match."))
		return zhttp.SeeOther(w, "/user/auth")
	}

	err = u.UpdatePassword(r.Context(), args.Password)
	if err != nil {
		var vErr *zvalidate.Validator
		if errors.As(err, &vErr) {
			zhttp.FlashError(w, r, fmt.Sprintf("%s", err))
			return zhttp.SeeOther(w, "/user/auth")
		}
		return err
	}

	zhttp.Flash(w, r, T(r.Context(), "notify/password-changed|Password changed."))
	return zhttp.SeeOther(w, "/user/auth")
}

func (h user) resendVerify(w http.ResponseWriter, r *http.Request) error {
	user := User(r.Context())
	if user.EmailVerified {
		zhttp.Flash(w, r, T(r.Context(), "notify/email-already-verified|%(email) is already verified.", user.Email))
		return zhttp.SeeOther(w, "/")
	}

	sendEmailVerify(r.Context(), Site(r.Context()), user, goatcounter.Config(r.Context()).EmailFrom)
	zhttp.Flash(w, r, T(r.Context(), "notify/sent-to-email|Sent to %(email).", user.Email))
	return zhttp.SeeOther(w, "/")
}

func sendEmailVerify(ctx context.Context, site *goatcounter.Site, user *goatcounter.User, emailFrom string) {
	ctx = goatcounter.CopyContextValues(ctx)
	bgrun.RunFunction("email:verify", func() {
		err := blackmail.Send("Verify your email",
			mail.Address{Name: "GoatCounter", Address: emailFrom},
			blackmail.To(user.Email),
			blackmail.BodyMustText(goatcounter.TplEmailVerify{ctx, *site, *user}.Render))
		if err != nil {
			log.Errorf(ctx, "blackmail: %s", err)
		}
	})
}

func (h user) verify(w http.ResponseWriter, r *http.Request) error {
	var (
		key  = chi.URLParam(r, "key")
		user goatcounter.User
	)
	err := user.ByEmailToken(r.Context(), key)
	// It's not a problem if the user exists and is already activated. Usually
	// happens because browser "prefetches" the URL, or people accidentally
	// opening the link twice, or something like that.
	if err != nil && zdb.ErrNoRows(err) && r.URL.Query().Get("email") != "" {
		_ = user.ByEmail(r.Context(), r.URL.Query().Get("email"))
		if user.EmailVerified {
			err = nil
		}
	}
	if err != nil {
		if zdb.ErrNoRows(err) {
			return guru.New(400, T(r.Context(), "error/unknown-activation-token|Unknown activation token"))
		}
		return err
	}

	err = user.VerifyEmail(r.Context())
	if err != nil {
		return err
	}
	zhttp.Flash(w, r, fmt.Sprintf("%q verified", user.Email))
	return zhttp.SeeOther(w, "/")
}

// Make sure to use the correct cookie, since both "custom.example.com" and
// "example.goatcounter.com" will work if you're using a custom domain.
func cookieDomain(site *goatcounter.Site, r *http.Request) string {
	if r.Host == site.Domain(r.Context()) {
		return site.Domain(r.Context())
	}
	return goatcounter.Config(r.Context()).Domain
}
