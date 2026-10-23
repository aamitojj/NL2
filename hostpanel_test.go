package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{{"0.1.0", "0.1.0", 0}, {"v0.2.0", "0.1.9", 1}, {"0.1", "0.1.1", -1}, {"1.0.0-beta", "0.9.9", 1}, {"0.10.0", "0.9.0", 1}}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCleanRel(t *testing.T) {
	for _, bad := range []string{"../x", "a/../../x", "/etc/passwd", "C:/win", `..\x`} {
		if _, err := cleanRel(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	if r, _ := cleanRel("__MACOSX/a"); r != "" {
		t.Error("should skip __MACOSX")
	}
	if r, _ := cleanRel(`site\css\a.css`); r != "site/css/a.css" {
		t.Errorf("got %q", r)
	}
}

func makeZip(t *testing.T, files map[string]string) string {
	p := filepath.Join(t.TempDir(), "s.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	for n, c := range files {
		w, _ := zw.Create(n)
		io.WriteString(w, c)
	}
	zw.Close()
	f.Close()
	return p
}

func tarNames(t *testing.T, p string) []string {
	f, _ := os.Open(p)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	sort.Strings(names)
	return names
}

func TestZipStripWrapper(t *testing.T) {
	z := makeZip(t, map[string]string{"proj/dist/index.html": "hi", "proj/dist/css/a.css": "x", "__MACOSX/proj/._index.html": "junk"})
	e, cl, err := zipEntries(z)
	if err != nil {
		t.Fatal(err)
	}
	defer cl()
	tb, n, _, err := buildSiteTarball(e)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tb)
	got := tarNames(t, tb)
	if n != 2 || got[0] != "css/a.css" || got[1] != "index.html" {
		t.Fatalf("got %v", got)
	}
}

func TestZipSlipRejected(t *testing.T) {
	z := makeZip(t, map[string]string{"index.html": "a", "../../evil.sh": "x"})
	if _, _, err := zipEntries(z); err == nil {
		t.Fatal("zip slip not rejected")
	}
}

func TestNoIndexRejected(t *testing.T) {
	z := makeZip(t, map[string]string{"a/readme.txt": "a", "b/x.html": "x"})
	e, cl, _ := zipEntries(z)
	defer cl()
	if _, _, _, err := buildSiteTarball(e); err == nil {
		t.Fatal("expected missing index.html error")
	}
}

func TestDomains(t *testing.T) {
	d, err := normalizeDomain("https://WWW.Example.com/path")
	if err != nil || d != "www.example.com" {
		t.Fatalf("%q %v", d, err)
	}
	if _, err := normalizeDomain("not a domain"); err == nil {
		t.Fatal("expected error")
	}
	r := requiredRecords("example.com", true, "1.2.3.4")
	if len(r) != 2 || r[0].Name != "@" || r[1].Name != "www" || r[0].Value != "1.2.3.4" {
		t.Fatalf("%+v", r)
	}
	r = requiredRecords("shop.example.com", false, "1.2.3.4")
	if r[0].Name != "shop" {
		t.Fatalf("%+v", r)
	}
	r = requiredRecords("example.co.uk", false, "1.2.3.4")
	if r[0].Name != "@" {
		t.Fatalf("%+v", r)
	}
}

func TestSlug(t *testing.T) {
	if s := slugify("Bakery Smith & Sons!"); s != "bakery-smith-sons" {
		t.Fatal(s)
	}
	if validSlug("-x") || validSlug("A") || !validSlug("a1-b") {
		t.Fatal("slug validation")
	}
}

func TestShellQuote(t *testing.T) {
	if shellQuote("it's") != `'it'"'"'s'` {
		t.Fatal(shellQuote("it's"))
	}
}
