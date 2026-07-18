package declare

import (
	"strings"
	"testing"
)

// TestParsePluginsBlock covers the declared plugins block: the accepted shape
// with defaults, and each refusal naming its offending piece.
func TestParsePluginsBlock(t *testing.T) {
	base := `name: mailer
run: [python, main.py]
plugins:
`
	tests := []struct {
		name    string
		plugins string
		wantErr string
	}{
		{
			name: "valid with lifetime and default",
			plugins: `  mail:
    ref: smtp-send@1.0
  browser:
    ref: lightpanda@0.4
    lifetime: resident
`,
		},
		{
			name:    "not a mapping",
			plugins: `  - mail`,
			wantErr: `field "plugins" must be a mapping`,
		},
		{
			name:    "bad alias",
			plugins: "  Mail.Now:\n    ref: smtp-send@1.0\n",
			wantErr: "not a lowercase slug",
		},
		{
			name:    "entry not a mapping",
			plugins: "  mail: smtp-send@1.0\n",
			wantErr: `plugin "mail" must be a mapping`,
		},
		{
			name:    "unknown field",
			plugins: "  mail:\n    ref: smtp-send@1.0\n    fresh: true\n",
			wantErr: `unknown field "fresh"`,
		},
		{
			name:    "missing ref",
			plugins: "  mail:\n    lifetime: run\n",
			wantErr: `needs a non-empty "ref"`,
		},
		{
			name:    "ref not name@version",
			plugins: "  mail:\n    ref: smtp-send\n",
			wantErr: "is not name@version",
		},
		{
			name:    "bad lifetime",
			plugins: "  mail:\n    ref: smtp-send@1.0\n    lifetime: forever\n",
			wantErr: "lifetime must be run, lane, or resident",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseDeclaration([]byte(base + tt.plugins))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDeclaration: %v", err)
			}
			p := d.Pipeline
			if len(p.Plugins) != 2 {
				t.Fatalf("plugins = %+v", p.Plugins)
			}
			if got := p.Plugins["mail"]; got.Ref != "smtp-send@1.0" || got.EffectiveLifetime() != LifetimeRun {
				t.Fatalf("mail binding = %+v", got)
			}
			if got := p.Plugins["browser"]; got.Ref != "lightpanda@0.4" || got.EffectiveLifetime() != LifetimeResident {
				t.Fatalf("browser binding = %+v", got)
			}
		})
	}
}

// TestPluginsAbsent keeps the no-plugins declaration exactly as before.
func TestPluginsAbsent(t *testing.T) {
	d, err := ParseDeclaration([]byte("name: quiet\nrun: [./run]\n"))
	if err != nil {
		t.Fatalf("ParseDeclaration: %v", err)
	}
	if len(d.Pipeline.Plugins) != 0 {
		t.Fatalf("plugins = %+v", d.Pipeline.Plugins)
	}
}
