package main

// Zero-touch server setup. Everything here was checked against Coolify's own
// source (github.com/coollabsio/coolify) and install.sh; see docs/AUTOMATION.md.
//
//  - install.sh reads ROOT_USERNAME / ROOT_USER_EMAIL / ROOT_USER_PASSWORD and
//    writes them to /data/coolify/source/.env; at container start the
//    "start:seeder" s6 service runs ProductionSeeder -> RootUserSeeder, which
//    creates the admin (user id 0, owner of team 0) and disables registration.
//  - The API is off by default in production (InstanceSettingsSeeder:
//    is_api_enabled = isDev()); ApiAllowed middleware returns 403 until it is on.
//  - Tokens are Sanctum tokens: User::createToken($name, $abilities,
//    $expiresAt, $teamId) (app/Models/User.php), the same call the
//    "Keys & Tokens" page makes.
//  - GET /api/health returns "OK" without auth (routes/api.php).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// genPassword returns a password that passes Coolify's RootUserSeeder rules
// (min 8, mixed case, letters, numbers, symbols, not in breach lists). The
// symbol set avoids characters that break install.sh's `sed s|..|..|` or
// .env parsing ( | & \ # $ quotes and spaces ).
func genPassword(n int) string {
	const lower = "abcdefghijkmnopqrstuvwxyz"
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const digits = "23456789"
	const symbols = "-_.+:"
	all := lower + upper + digits + symbols
	pick := func(set string) byte {
		i, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		return set[i.Int64()]
	}
	b := []byte{pick(lower), pick(upper), pick(digits), pick(symbols)}
	for len(b) < n {
		b = append(b, pick(all))
	}
	for i := len(b) - 1; i > 0; i-- { // shuffle
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		b[i], b[j.Int64()] = b[j.Int64()], b[i]
	}
	return string(b)
}

// sslipHost is the free instant host name for a site: sslip.io answers any
// name that embeds an IP with that IP, so it works without any DNS setup.
// IPv6 uses dashes, like Coolify's sslipHostLabel().
func sslipHost(slug, ip string) string {
	if p := net.ParseIP(ip); p != nil && p.To4() == nil {
		ip = strings.ReplaceAll(p.String(), ":", "-")
		if strings.HasPrefix(ip, "-") {
			ip = "0" + ip
		}
		if strings.HasSuffix(ip, "-") {
			ip += "0"
		}
	}
	return slug + "." + ip + ".sslip.io"
}

func defaultAdminEmail(ip string) string {
	// Must pass Laravel's email:rfc,dns rule: <ip>.sslip.io has an A record.
	return "admin@" + strings.ReplaceAll(ip, ":", "-") + ".sslip.io"
}

// ensureManagedKey returns Dilreet's own ed25519 key, creating it once.
func ensureManagedKey() (ssh.Signer, string, error) {
	c := snapshotConfig()
	if c.ManagedKey != "" {
		s, err := ssh.ParsePrivateKey([]byte(secret(c.ManagedKey)))
		if err == nil {
			return s, c.ManagedKeyPub, nil
		}
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "dilreet")
	if err != nil {
		return nil, "", err
	}
	pemStr := string(pem.EncodeToMemory(blk))
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", err
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " dilreet"
	enc, err := protectString(pemStr)
	if err != nil {
		return nil, "", err
	}
	err = withConfig(func(c *Config) error { c.ManagedKey, c.ManagedKeyPub = enc, pub; return nil })
	return signer, pub, err
}

// installManagedKey adds Dilreet's public key to the SSH user's
// authorized_keys so later sessions need no password (key auth, as the
// security rules require: the password itself is never stored).
func installManagedKey(ctx context.Context) error {
	_, pub, err := ensureManagedKey()
	if err != nil {
		return err
	}
	user := snapshotConfig().SSHUser
	script := fmt.Sprintf(`set -e
u=%s; k=%s
h=$(getent passwd "$u" | cut -d: -f6); [ -n "$h" ] || h=/root
mkdir -p "$h/.ssh"; chmod 700 "$h/.ssh"; touch "$h/.ssh/authorized_keys"; chmod 600 "$h/.ssh/authorized_keys"
grep -qxF "$k" "$h/.ssh/authorized_keys" || echo "$k" >> "$h/.ssh/authorized_keys"
chown -R "$u": "$h/.ssh" 2>/dev/null || true
echo HP_KEY_OK`, shellQuote(user), shellQuote(pub))
	out, err := runRemoteOutput(ctx, script)
	if err != nil || !strings.Contains(out, "HP_KEY_OK") {
		return fmt.Errorf("could not add Dilreet's key to the server: %v %s", err, strings.TrimSpace(out))
	}
	return nil
}

// ---------- server preparation ----------

// Coolify's install.sh only accepts these /etc/os-release IDs (plus a few
// aliases it maps: pop, linuxmint, zorin -> ubuntu, cachyos -> arch, ...).
var coolifyOS = []string{"arch", "archarm", "ubuntu", "debian", "raspbian", "centos", "fedora", "rhel", "ol", "rocky", "sles", "opensuse-leap", "opensuse-tumbleweed", "almalinux", "amzn", "alpine", "postmarketos", "tencentos", "pop", "linuxmint", "zorin", "cachyos", "manjaro", "manjaro-arm", "endeavouros", "fedora-asahi-remix"}

type ServerFacts struct {
	OSID      string `json:"os_id"`
	OSName    string `json:"os_name"`
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`
	RAMMB     int    `json:"ram_mb"`
	SwapMB    int    `json:"swap_mb"`
	DiskFreeG int    `json:"disk_free_gb"`
	Coolify   bool   `json:"coolify_running"`
	UFW       bool   `json:"ufw_active"`
	Supported bool   `json:"supported"`
}

const factsScript = `. /etc/os-release 2>/dev/null
echo "id=$ID"; echo "name=$PRETTY_NAME"; echo "arch=$(uname -m)"; echo "cpus=$(nproc 2>/dev/null)"
echo "ram=$(free -m 2>/dev/null | awk '/Mem:/{print $2}')"; echo "swap=$(free -m 2>/dev/null | awk '/Swap:/{print $2}')"
echo "disk=$(df -BG / 2>/dev/null | awk 'NR==2{gsub("G","",$4); print $4}')"
if command -v docker >/dev/null && docker ps --format '{{.Names}}' 2>/dev/null | grep -qx coolify; then echo coolify=1; else echo coolify=0; fi
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then echo ufw=1; else echo ufw=0; fi`

func parseFacts(out string) ServerFacts {
	var f ServerFacts
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		n := atoiSafe(v)
		switch k {
		case "id":
			f.OSID = strings.Trim(v, `"`)
		case "name":
			f.OSName = strings.Trim(v, `"`)
		case "arch":
			f.Arch = v
		case "cpus":
			f.CPUs = n
		case "ram":
			f.RAMMB = n
		case "swap":
			f.SwapMB = n
		case "disk":
			f.DiskFreeG = n
		case "coolify":
			f.Coolify = v == "1"
		case "ufw":
			f.UFW = v == "1"
		}
	}
	for _, id := range coolifyOS {
		if f.OSID == id {
			f.Supported = true
		}
	}
	return f
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func serverFacts(ctx context.Context) (ServerFacts, error) {
	out, err := runRemoteOutput(ctx, factsScript)
	if err != nil {
		return ServerFacts{}, err
	}
	return parseFacts(out), nil
}

// prepScript: swap when RAM is small, open Coolify's ports if ufw is active
// (never enables a firewall that is off: that could lock the user out), and
// optional unattended security updates on Debian/Ubuntu.
func prepScript(sshPort int, ramMB, swapMB int, autoUpdates bool, osID string) string {
	var b strings.Builder
	b.WriteString("set -u\n")
	if ramMB > 0 && ramMB < 3800 && swapMB < 512 {
		b.WriteString(`if ! swapon --show 2>/dev/null | grep -q . ; then
  echo "Adding a 2 GB swap file (this server has little memory)"
  (fallocate -l 2G /swapfile 2>/dev/null || dd if=/dev/zero of=/swapfile bs=1M count=2048 status=none) && chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile && (grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab) && echo "Swap enabled" || echo "Could not add swap (continuing)"
fi
`)
	}
	fmt.Fprintf(&b, `if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  echo "Opening firewall ports for Coolify (ufw)"
  for p in %d 22 80 443 8000 6001 6002; do ufw allow "$p/tcp" >/dev/null; done
  ufw allow 443/udp >/dev/null 2>&1 || true
  echo "Firewall ports open: %d, 22, 80, 443, 8000, 6001, 6002"
else
  echo "No active ufw firewall (if your provider has a cloud firewall, allow ports 22, 80, 443 and 8000 there)"
fi
`, sshPort, sshPort)
	if autoUpdates && (osID == "ubuntu" || osID == "debian" || osID == "raspbian") {
		b.WriteString(`echo "Turning on automatic security updates"
export DEBIAN_FRONTEND=noninteractive
(apt-get -qq update && apt-get -qq install -y unattended-upgrades >/dev/null) && printf 'APT::Periodic::Update-Package-Lists "1";\nAPT::Periodic::Unattended-Upgrade "1";\n' > /etc/apt/apt.conf.d/20auto-upgrades && echo "Automatic security updates are on" || echo "Could not enable automatic updates (continuing)"
`)
	}
	b.WriteString("echo HP_PREP_DONE\n")
	return b.String()
}

// ---------- Coolify connection ----------

// coolifyConnectPHP is run with `docker exec -i coolify php` (script on
// stdin, so no secret is ever on a command line). It mirrors what Coolify does
// itself: RootUserSeeder / CreateNewUser for the admin, InstanceSettings for
// the API switch and User::createToken for the token.
const coolifyConnectPHP = `<?php
require '/var/www/html/vendor/autoload.php';
$app = require '/var/www/html/bootstrap/app.php';
$app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();
use App\Models\User; use App\Models\Team; use App\Models\InstanceSettings;
$cfg = json_decode(base64_decode('%s'), true);
$out = ['ok' => true];
try {
  $u = User::find(0);
  if (!$u && !empty($cfg['create_admin']) && User::count() == 0) {
    $u = (new User)->forceFill(['id' => 0, 'name' => $cfg['name'], 'email' => $cfg['email'], 'password' => Illuminate\Support\Facades\Hash::make($cfg['password'])]);
    $u->save();
    $team = $u->teams()->first() ?? Team::find(0);
    if ($team !== null && !$u->teams()->where('team_id', $team->id)->exists()) { $u->teams()->attach($team, ['role' => 'owner']); }
    $s = InstanceSettings::find(0); if ($s) { $s->is_registration_enabled = false; $s->save(); }
    $out['admin_created'] = true;
  }
  if (!$u) { echo 'HPRESULT:' . json_encode(['ok' => false, 'error' => 'no_admin', 'users' => User::count()]) . "\n"; exit(0); }
  $out['admin_email'] = $u->email;
  $s = InstanceSettings::find(0);
  if ($s) { $s->is_api_enabled = true; $s->save(); }
  $u->tokens()->whereIn('name', ['HostPanel', 'Dilreet'])->delete();
  $team = $u->teams()->first();
  $tok = $u->createToken('Dilreet', ['read', 'write', 'deploy'], null, $team ? $team->id : 0);
  $out['token'] = $tok->plainTextToken;
} catch (\Throwable $e) { $out = ['ok' => false, 'error' => $e->getMessage()]; }
echo 'HPRESULT:' . json_encode($out) . "\n";
`

type connectResult struct {
	OK           bool   `json:"ok"`
	Error        string `json:"error"`
	Token        string `json:"token"`
	AdminEmail   string `json:"admin_email"`
	AdminCreated bool   `json:"admin_created"`
	Users        int    `json:"users"`
}

// runCoolifyPHP uploads the script as a 0600 file, pipes it into PHP inside
// the coolify container and deletes it. The output is never logged.
func runCoolifyPHP(ctx context.Context, cfg map[string]any) (*connectResult, error) {
	payload, _ := json.Marshal(cfg)
	script := fmt.Sprintf(coolifyConnectPHP, base64.StdEncoding.EncodeToString(payload))
	p, err := uploadSecret([]byte(script))
	if err != nil {
		return nil, err
	}
	out, err := runRemoteOutput(ctx, fmt.Sprintf(`f=%s; trap 'rm -f "$f"' EXIT; docker exec -i coolify php < "$f"`, shellQuote(p)))
	i := strings.LastIndex(out, "HPRESULT:")
	if i < 0 {
		tail := strings.TrimSpace(out)
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		if err == nil {
			err = errors.New("no result")
		}
		return nil, fmt.Errorf("could not talk to Coolify inside the server (%v): %s", err, tail)
	}
	line := out[i+len("HPRESULT:"):]
	if k := strings.IndexByte(line, '\n'); k >= 0 {
		line = line[:k]
	}
	var r connectResult
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return nil, fmt.Errorf("unexpected answer from Coolify: %w", err)
	}
	return &r, nil
}

// waitCoolifyHealthy polls Coolify's unauthenticated health endpoint from the
// server itself.
func waitCoolifyHealthy(ctx context.Context, j *Job, maxWait time.Duration) error {
	tries := int(maxWait / (4 * time.Second))
	script := fmt.Sprintf(`for i in $(seq 1 %d); do if curl -fsS -m 3 http://127.0.0.1:8000/api/health 2>/dev/null | grep -q OK; then echo HP_UP; exit 0; fi; sleep 4; done; echo HP_DOWN; exit 1`, tries)
	out, err := runRemoteOutput(ctx, script)
	if strings.Contains(out, "HP_UP") {
		return nil
	}
	if j != nil {
		j.Logf("Coolify did not answer on http://127.0.0.1:8000/api/health in time.")
	}
	if err == nil {
		err = errors.New("timeout")
	}
	return fmt.Errorf("Coolify is not responding yet (%v). Wait a minute and click Retry", err)
}

// autoConnectCoolify enables the API and creates Dilreet's token over SSH,
// then saves and tests it. createAdmin=true allows creating the admin itself
// if Coolify's seeder did not (e.g. it rejected the e-mail).
func autoConnectCoolify(ctx context.Context, j *Job, createAdmin bool) error {
	c := snapshotConfig()
	cfg := map[string]any{"create_admin": false}
	if createAdmin && c.AdminEmail != "" && c.AdminPassword != "" {
		cfg = map[string]any{"create_admin": false, "name": "Dilreet Admin", "email": c.AdminEmail, "password": secret(c.AdminPassword)}
	}
	var r *connectResult
	var err error
	// The admin is created by the seeder when the container starts, which can
	// take a little while after /api/health is up. Wait for it before
	// creating it ourselves.
	for attempt := 0; attempt < 13; attempt++ {
		if attempt == 12 && createAdmin && cfg["email"] != nil {
			cfg["create_admin"] = true
			j.Logf("Coolify did not create the admin account by itself; creating it now.")
		}
		r, err = runCoolifyPHP(ctx, cfg)
		if err != nil {
			if attempt < 3 {
				time.Sleep(5 * time.Second)
				continue
			}
			return err
		}
		if r.OK || r.Error != "no_admin" {
			break
		}
		if !createAdmin && r.Users == 0 {
			return errors.New("Coolify has no admin account yet. Open Coolify once to register, or use 'Set up automatically'")
		}
		time.Sleep(5 * time.Second)
	}
	if r == nil || !r.OK {
		msg := "unknown error"
		if r != nil {
			msg = r.Error
		}
		return fmt.Errorf("could not create the API token automatically: %s", msg)
	}
	enc, err := protectString(r.Token)
	if err != nil {
		return err
	}
	err = withConfig(func(c *Config) error {
		c.CoolifyToken = enc
		c.APIViaSSH = true // the token never travels over the internet
		if c.CoolifyURL == "" {
			c.CoolifyURL = "http://" + c.ServerIP + ":8000"
		}
		if r.AdminEmail != "" && c.AdminEmail == "" {
			c.AdminEmail = r.AdminEmail
		}
		return nil
	})
	if err != nil {
		return err
	}
	if r.AdminCreated {
		j.Logf("Admin account created.")
	}
	j.Logf("API enabled and a Dilreet token created (stored %s).", secretStorageNote)
	// Test it, retrying briefly: the API can need a moment after enabling.
	return retry(ctx, 5, 3*time.Second, func() error {
		api, err := newCoolify()
		if err != nil {
			return err
		}
		v, err := api.Version()
		if err != nil {
			return err
		}
		srv, err := api.Servers()
		if err != nil {
			return err
		}
		if len(srv) > 0 {
			_ = withConfig(func(c *Config) error {
				if c.ServerUUID == "" {
					c.ServerUUID = srv[0].UUID
				}
				return nil
			})
		}
		j.Logf("Connected to Coolify %s.", v)
		return nil
	})
}

// retry runs fn up to n times with a growing pause, but only while the error
// looks transient (network trouble, 5xx, 429).
func retry(ctx context.Context, n int, pause time.Duration, fn func() error) error {
	var err error
	for i := 0; i < n; i++ {
		if err = fn(); err == nil || !isTransient(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pause * time.Duration(i+1)):
		}
	}
	return err
}

func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 429 || ae.Status >= 500
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{"connection reset", "connection refused", "timeout", "eof", "broken pipe", "no route", "temporarily", "cannot reach"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// ---------- the one-click setup job ----------

type SetupOptions struct {
	AdminEmail  string `json:"admin_email"`
	AutoUpdates bool   `json:"auto_updates"`
	RememberKey bool   `json:"remember_key"`
}

var setupSteps = []string{"Checking your server", "Preparing the server", "Installing the hosting engine", "Starting the engine", "Connecting Dilreet", "Ready"}

func setupServerJob(j *Job, o SetupOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()
	j.SetSteps(setupSteps...)
	j.Step(0)
	f, err := serverFacts(ctx)
	if err != nil {
		return err
	}
	j.Logf("Server: %s, %s, %d CPU, %d MB RAM, %d GB free disk.", f.OSName, f.Arch, f.CPUs, f.RAMMB, f.DiskFreeG)
	if !f.Supported && !f.Coolify {
		return fmt.Errorf("this server runs %q, which Coolify's installer does not support. Reinstall the server with Ubuntu 24.04 LTS (recommended) and try again", f.OSName)
	}
	if f.DiskFreeG > 0 && f.DiskFreeG < 20 {
		j.Logf("Warning: only %d GB free disk; Coolify recommends at least 30 GB.", f.DiskFreeG)
	}
	if o.RememberKey {
		if err := installManagedKey(ctx); err != nil {
			j.Logf("Note: %v", err)
		} else {
			j.Logf("Password-free login set up (Dilreet's own key; your password is not stored).")
		}
	}

	j.Step(1)
	c := snapshotConfig()
	_ = withConfig(func(c *Config) error { c.AutoSecurityUpd = o.AutoUpdates; return nil })
	if err := runRemote(ctx, prepScript(c.SSHPort, f.RAMMB, f.SwapMB, o.AutoUpdates, f.OSID), j); err != nil {
		j.Logf("Server preparation reported a problem (%v); continuing.", err)
	}

	createdAdmin := false
	if !f.Coolify {
		j.Step(2)
		email := strings.TrimSpace(o.AdminEmail)
		if email == "" {
			email = defaultAdminEmail(c.ServerIP)
		}
		pw := genPassword(24)
		encPW, err := protectString(pw)
		if err != nil {
			return err
		}
		if err := withConfig(func(c *Config) error { c.AdminEmail, c.AdminPassword = email, encPW; return nil }); err != nil {
			return err
		}
		env := fmt.Sprintf("ROOT_USERNAME=%s\nROOT_USER_EMAIL=%s\nROOT_USER_PASSWORD=%s\n", shellQuote("Dilreet Admin"), shellQuote(email), shellQuote(pw))
		envPath, err := uploadSecret([]byte(env))
		if err != nil {
			return err
		}
		j.Logf("Installing Coolify with the official installer; Dilreet pre-creates your admin account (%s). This usually takes 2-10 minutes.", email)
		j.Logf("$ %s", coolifyInstallCmd)
		j.Span(25, 75)
		j.SetProgress(0, "Installing the hosting engine")
		j.mu.Lock()
		j.onLine = installLineHook
		j.mu.Unlock()
		cmd := fmt.Sprintf(`f=%s; set -a; . "$f"; set +a; rm -f "$f"; %s`, shellQuote(envPath), coolifyInstallCmd)
		err = runRemote(ctx, cmd, j)
		j.mu.Lock()
		j.onLine = nil
		j.mu.Unlock()
		j.Span(0, 100)
		_, _ = runRemoteOutput(ctx, "rm -f "+shellQuote(envPath))
		if err != nil {
			return err
		}
		createdAdmin = true
	} else {
		j.Logf("Coolify is already installed on this server; skipping the installation.")
	}

	j.Step(3)
	if err := waitCoolifyHealthy(ctx, j, 6*time.Minute); err != nil {
		return err
	}
	j.Step(4)
	if err := autoConnectCoolify(ctx, j, createdAdmin); err != nil {
		return err
	}
	j.Step(5)
	j.Logf("All set. Drop a folder or zip on the Sites page to publish your first site.")
	return nil
}
