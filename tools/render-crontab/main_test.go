package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseJob(t *testing.T) {
	var tests = map[string]struct {
		line     string
		schedule string
		command  string
		wantErr  string
	}{
		"simple":            {line: "15 0 * * *  index-discovery", schedule: "15 0 * * *", command: "index-discovery"},
		"args keep spacing": {line: "0 2 * * 0\tsubscription-send  -f W", schedule: "0 2 * * 0", command: "subscription-send  -f W"},
		"keyword":           {line: "@daily index-discovery -b", schedule: "@daily", command: "index-discovery -b"},
		"lists and steps":   {line: "*/15 4,12,20 1-7 jan-mar mon-fri find /tmp", schedule: "*/15 4,12,20 1-7 jan-mar mon-fri", command: "find /tmp"},
		"quoted chain":      {line: `0 4 * * * sh -c 'dspace a && dspace b'`, schedule: "0 4 * * *", command: `sh -c 'dspace a && dspace b'`},
		"escaped percent":   {line: `0 4 * * * date +\%F`, schedule: "0 4 * * *", command: `date +\%F`},

		"no command":          {line: "0 1 * * *", wantErr: "no command"},
		"missing field":       {line: "0 2 * * subscription-send -f D", wantErr: "day of week"},
		"name in wrong field": {line: "0 jan * * * cleanup", wantErr: "hour"},
		"bad keyword":         {line: "@sometimes cleanup", wantErr: "keyword"},
		"redirect":            {line: "15 0 * * * oai import > /dev/null", wantErr: `'>'`},
		"chain":               {line: "0 4 * * * dspace a; dspace b", wantErr: `';'`},
		"pipe":                {line: "0 4 * * * dspace a | tail", wantErr: `'|'`},
		"host var":            {line: "0 4 * * * find $HOME", wantErr: `'$'`},
		"var in dquotes":      {line: `0 4 * * * sh -c "echo $HOME"`, wantErr: `'$'`},
		"bare percent":        {line: "0 4 * * * date +%F", wantErr: `"%"`},
		"percent in quotes":   {line: `0 4 * * * sh -c 'date +%F'`, wantErr: `"%"`},
		"unclosed quote":      {line: `0 4 * * * sh -c 'dspace a`, wantErr: "unclosed"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			sched, cmd, err := parseJob(tc.line)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if sched != tc.schedule || cmd != tc.command {
				t.Fatalf("got (%q, %q), want (%q, %q)", sched, cmd, tc.schedule, tc.command)
			}
		})
	}
}

func TestRender(t *testing.T) {
	var schedule = strings.Join([]string{
		"# Header about editing this file",
		"#",
		"",
		"#---",
		"# Daily",
		"15 0 * * *  index-discovery",
		"",
		"# 0 4 * * 0  checker -l -p",
		"@weekly  subscription-send -f W",
	}, "\n")
	var opts = options{projectDir: "/srv/sb", path: "/usr/local/bin:/usr/bin:/bin", mailto: "ops@example.org"}

	t.Run("user crontab", func(t *testing.T) {
		var out, errs = render([]byte(schedule), opts)
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		for _, want := range []string{
			"# BEGIN DSpace jobs for /srv/sb\n",
			"\nPATH=/usr/local/bin:/usr/bin:/bin\nMAILTO=ops@example.org\n",
			"\n\n#---\n# Daily\n15 0 * * *\t/srv/sb/scripts/cron-job index-discovery\n",
			"\n# 0 4 * * 0  checker -l -p\n@weekly\t/srv/sb/scripts/cron-job subscription-send -f W\n",
			"\n# END DSpace jobs for /srv/sb\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output is missing %q; got:\n%s", want, out)
			}
		}
		if strings.Contains(out, "Header about editing") {
			t.Errorf("the schedule's header comment should be dropped; got:\n%s", out)
		}
	})

	t.Run("system crontab", func(t *testing.T) {
		var sysOpts = opts
		sysOpts.user = "dspace"
		var out, errs = render([]byte(schedule), sysOpts)
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		var want = "15 0 * * *\tdspace\t/srv/sb/scripts/cron-job index-discovery\n"
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q; got:\n%s", want, out)
		}
	})

	t.Run("no mailto", func(t *testing.T) {
		var noMail = opts
		noMail.mailto = ""
		var out, _ = render([]byte(schedule), noMail)
		if strings.Contains(out, "MAILTO") {
			t.Errorf("MAILTO should be left out when unset; got:\n%s", out)
		}
	})

	t.Run("reports every bad line", func(t *testing.T) {
		var _, errs = render([]byte("0 1 * * *\nok\n15 0 * * * x > y\n"), opts)
		if len(errs) != 3 {
			t.Fatalf("want 3 errors, got %d: %v", len(errs), errs)
		}
		for i, prefix := range []string{"1: ", "2: ", "3: "} {
			if !strings.HasPrefix(errs[i].Error(), prefix) {
				t.Errorf("error %d should start with %q: %s", i, prefix, errs[i])
			}
		}
	})
}

func TestReadMailto(t *testing.T) {
	var tests = map[string]struct {
		env     string
		want    string
		wantErr bool
	}{
		"plain":         {env: "FOO=1\nCRON_MAILTO=ops@example.org\n", want: "ops@example.org"},
		"double quoted": {env: `CRON_MAILTO="ops@example.org"`, want: "ops@example.org"},
		"single quoted": {env: `CRON_MAILTO='ops@example.org'`, want: "ops@example.org"},
		"last wins":     {env: "CRON_MAILTO=a@example.org\nCRON_MAILTO=b@example.org\n", want: "b@example.org"},
		"commented out": {env: "#CRON_MAILTO=ops@example.org\n", want: ""},
		"unset":         {env: "FOO=1\n", want: ""},
		"interpolated":  {env: "CRON_MAILTO=${MAIL_ADMIN}\n", wantErr: true},
		"trailing note": {env: "CRON_MAILTO=ops@example.org # the ops list\n", wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var envFile = filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(envFile, []byte(tc.env), 0644); err != nil {
				t.Fatal(err)
			}
			var got, err = readMailto(envFile)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("no .env", func(t *testing.T) {
		var got, err = readMailto(filepath.Join(t.TempDir(), ".env"))
		if got != "" || err != nil {
			t.Fatalf("got (%q, %v), want (\"\", nil)", got, err)
		}
	})
}
