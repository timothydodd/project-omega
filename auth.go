package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	authCookie     = "omega_session"
	authSessionTTL = 30 * 24 * time.Hour
	pbkdf2Rounds   = 600_000
)

type user struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Rounds, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256:%d:%s:%s", pbkdf2Rounds,
		base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(key)), nil
}

func verifyPassword(password, stored string) bool {
	parts := strings.Split(stored, ":")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	rounds, err1 := strconv.Atoi(parts[1])
	salt, err2 := base64.StdEncoding.DecodeString(parts[2])
	want, err3 := base64.StdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, rounds, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func hasUsers() bool {
	r, _ := rowOf(db, "SELECT 1 x FROM users LIMIT 1")
	return r != nil
}

func createUser(email, password string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	_, err = db.Exec("INSERT INTO users (email, password_hash, created_at) VALUES (?, ?, ?)",
		strings.ToLower(strings.TrimSpace(email)), hash, time.Now().UnixMilli())
	return err
}

func login(w http.ResponseWriter, r *http.Request, email, password string) bool {
	u, _ := rowOf(db, "SELECT id, password_hash FROM users WHERE email = ?", strings.ToLower(strings.TrimSpace(email)))
	if u == nil || !verifyPassword(password, str(u, "password_hash")) {
		return false
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	db.Exec("INSERT INTO auth_sessions (token, user_id, expires_at) VALUES (?, ?, ?)", token, i64(u, "id"), now.Add(authSessionTTL).UnixMilli())
	db.Exec("DELETE FROM auth_sessions WHERE expires_at < ?", now.UnixMilli())
	setAuthCookie(w, r, token, int(authSessionTTL.Seconds()))
	return true
}

func logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(authCookie); err == nil {
		db.Exec("DELETE FROM auth_sessions WHERE token = ?", c.Value)
	}
	setAuthCookie(w, r, "", -1)
}

func currentUser(r *http.Request) *user {
	c, err := r.Cookie(authCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	row, _ := rowOf(db, `SELECT u.id, u.email FROM auth_sessions a JOIN users u ON u.id = a.user_id
		WHERE a.token = ? AND a.expires_at > ?`, c.Value, time.Now().UnixMilli())
	if row == nil {
		return nil
	}
	return &user{ID: i64(row, "id"), Email: str(row, "email")}
}

func setAuthCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: authCookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}
