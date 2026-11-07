package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
)

// kdfParams records how a password hash was derived so that parameters can be
// strengthened later without invalidating existing hashes.
type kdfParams struct {
	Alg     string `json:"alg"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"` // KiB
	Threads uint8  `json:"threads"`
	KeyLen  uint32 `json:"key_len"`
}

var (
	// currentKDF follows RFC 9106's memory-constrained recommendation.
	currentKDF = kdfParams{Alg: "argon2id", Time: 3, Memory: 64 * 1024, Threads: 2, KeyLen: 32}
	// legacyKDF is what files without a "kdf" field were created with.
	legacyKDF = kdfParams{Alg: "argon2id", Time: 1, Memory: 64 * 1024, Threads: 2, KeyLen: 32}
)

func (k kdfParams) valid() bool {
	return k.Alg == "argon2id" && k.Time >= 1 && k.Time <= 10 && k.Memory >= 8*1024 && k.Memory <= 1024*1024 && k.Threads >= 1 && k.Threads <= 16 && k.KeyLen >= 16 && k.KeyLen <= 64
}

type credentials struct {
	Salt      string     `json:"salt"`
	Password  string     `json:"password"`
	TokenHash string     `json:"token_hash"`
	KDF       *kdfParams `json:"kdf,omitempty"`
	// Epoch changes whenever the password changes; browser sessions created
	// under another epoch are discarded.
	Epoch string `json:"epoch,omitempty"`
}

func (c credentials) params() kdfParams {
	if c.KDF == nil {
		return legacyKDF
	}
	return *c.KDF
}
func (c credentials) verify(password string) bool {
	return equal(hashPassword(password, c.Salt, c.params()), c.Password)
}
func (c credentials) needsRehash() bool { return c.params() != currentKDF }

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func hashPassword(password, salt string, k kdfParams) string {
	return hex.EncodeToString(argon2.IDKey([]byte(password), []byte(salt), k.Time, k.Memory, k.Threads, k.KeyLen))
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func validatePassword(password string) error {
	if len(password) < 12 {
		return errors.New("administrator password must contain at least 12 characters")
	}
	if len(password) > 1024 {
		return errors.New("administrator password must contain at most 1024 characters")
	}
	return nil
}

func newPasswordCredentials(password, tokenHashValue string) credentials {
	kdf := currentKDF
	salt := randomToken()
	return credentials{Salt: salt, Password: hashPassword(password, salt, kdf), TokenHash: tokenHashValue, KDF: &kdf, Epoch: randomToken()}
}

func credentialsPath(root string) string { return filepath.Join(root, "admin.json") }
func tokenPath(root string) string       { return filepath.Join(root, "admin.token") }

func loadCredentials(root string) (credentials, error) {
	b, err := os.ReadFile(credentialsPath(root))
	if err != nil {
		return credentials{}, errors.New("initialize administrator credentials before serving")
	}
	var c credentials
	if err = json.Unmarshal(b, &c); err != nil || c.Salt == "" || c.Password == "" || c.TokenHash == "" || (c.KDF != nil && !c.KDF.valid()) {
		return credentials{}, errors.New("invalid administrator credentials")
	}
	return c, nil
}

// writeAtomic replaces path with data so readers never see a partial file.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func saveCredentials(root string, c credentials) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writeAtomic(credentialsPath(root), b)
}

func Initialize(root, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err := os.Chmod(root, 0700); err != nil {
		return err
	}
	token := randomToken()
	b, err := json.Marshal(newPasswordCredentials(password, tokenHash(token)))
	if err != nil {
		return err
	}
	f, err := os.OpenFile(credentialsPath(root), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("credentials already exist or state directory is not writable")
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	f, err = os.OpenFile(tokenPath(root), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteString(token + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

// ChangePassword replaces the administrator password and signs out every
// browser session. The CLI token is unchanged.
func ChangePassword(root, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	c, err := loadCredentials(root)
	if err != nil {
		return err
	}
	if err = saveCredentials(root, newPasswordCredentials(password, c.TokenHash)); err != nil {
		return fmt.Errorf("cannot save administrator credentials: %w", err)
	}
	return nil
}

// RotateToken issues a new CLI token, stores it in admin.token and revokes the
// previous one. Browser sessions are unaffected.
func RotateToken(root string) (string, error) {
	c, err := loadCredentials(root)
	if err != nil {
		return "", err
	}
	token := randomToken()
	c.TokenHash = tokenHash(token)
	if err = writeAtomic(tokenPath(root), []byte(token+"\n")); err != nil {
		return "", fmt.Errorf("cannot write CLI token: %w", err)
	}
	if err = saveCredentials(root, c); err != nil {
		return "", fmt.Errorf("cannot save administrator credentials: %w", err)
	}
	return token, nil
}
