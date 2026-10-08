package sandbox

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/perm"
)

// TestSecretShapesMatchPerm: the seatbelt rule spells out the list perm's
// PathIsHiddenSecret owns, so a name is hidden from a confined command exactly
// when perm hides it.
func TestSecretShapesMatchPerm(t *testing.T) {
	var shapes []*regexp.Regexp
	for _, m := range regexp.MustCompile(`#"(.*?)"`).FindAllStringSubmatch(secretShapes, -1) {
		shapes = append(shapes, regexp.MustCompile(m[1]))
	}
	hides := func(name string) bool {
		return slices.ContainsFunc(shapes, func(re *regexp.Regexp) bool { return re.MatchString("/" + name) })
	}
	// a name each perm shape matches, in both spellings: perm folds the name it
	// is given, so a shape the rule misses in one case fails here
	for _, shape := range perm.SecretPathShape {
		name := strings.ReplaceAll(shape, "*", "x")
		for _, spelled := range []string{name, strings.ToUpper(name)} {
			if got, want := hides(spelled), perm.PathIsHiddenSecret(spelled); got != want {
				t.Errorf("shape %q (%s): the sandbox hides %v, perm %v", shape, spelled, got, want)
			}
		}
	}
	// the drift this pins: an id_ name takes a suffix, its .pub half is shared,
	// and a .env template is hidden here though grep may list it - folded, so
	// .ENV is a secret and an upper case .PUB is not
	for _, c := range []struct {
		name   string
		hidden bool
	}{
		{"id_rsa", true},
		{"id_rsa_old", true},
		{"ID_RSA_OLD", true},
		{"id_ed25519.pem", true},
		{"id_ed25519_backup", true},
		// short and pub-shaped tails, spelled out: an interval here is dead in the
		// engine, and the exact name comes back readable without this
		{"id_rsa1", true},
		{"id_rsab", true},
		{"id_rsaub", true},
		{"id_rsapub", true},
		{"id_rsa.pu", true},
		{"id_rsa.xpub", true},
		{"id_rsaXpub", true},
		{"id_rsa.pub", false},
		{"id_rsa-pub", true},
		{"ID_RSA.PUB", false},
		{"id_rsa.PUB", false},
		{"id_rsa.Pub", false},
		{"id_rsab.pub", false},
		{"id_rsa_old.pub", false},
		// the tail may not reach past a separator: perm judges the base name, and
		// pub here matches no shape
		{"id_rsa/pub", false},
		{".env", true},
		{".ENV", true},
		{".env.example", true},
		{".ENV.EXAMPLE", true},
		{".env.local", true},
		// one name per profile rule: the loop above walks perm's list, so a name
		// either side drops would otherwise stay green
		{".envrc", true},
		{".netrc", true},
		{".npmrc", true},
		{".yarnrc.yml", true},
		{".pgpass", true},
		{".pypirc", true},
		{".htpasswd", true},
		{".my.cnf", true},
		{"app.env", true},
		{"cert.pem", true},
		{"server.key", true},
		{"bundle.p12", true},
		{"cert.pfx", true},
		{"store.jks", true},
		{"app.keystore", true},
		{"main.go", false},
		{"MAIN.GO", false},
	} {
		if got, want := hides(c.name), perm.PathIsHiddenSecret(c.name); got != c.hidden || want != c.hidden {
			t.Errorf("%s: the sandbox hides %v, perm %v, want %v", c.name, got, want, c.hidden)
		}
	}
	// the engine ignores {n,m}, so an interval matches nothing there and the name
	// it covers reads back: the cases above cannot see it, only the profile text
	if strings.Contains(idShapes, "{") {
		t.Errorf("idShapes holds an interval, which the sandbox engine ignores")
	}
}
