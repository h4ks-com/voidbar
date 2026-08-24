package mirror

import "testing"

func TestSplitWayback(t *testing.T) {
	cases := []struct {
		base, original string
		ok             bool
	}{
		{
			base:     "https://web.archive.org/web/20220601000000id_/https://discord.com",
			original: "https://discord.com",
			ok:       true,
		},
		{
			base:     "https://web.archive.org/web/20231225120036id_/https://archive.org/download/item",
			original: "https://archive.org/download/item",
			ok:       true,
		},
		{base: "https://web.archive.org/web/20220601000000/https://discord.com", ok: false},
		{base: "https://web.archive.org/web/", ok: false},
		{base: "https://archive.org/download/voidbar-client", ok: false},
		{base: "https://discord.com", ok: false},
	}
	for _, tc := range cases {
		original, ok := SplitWayback(tc.base)
		if ok != tc.ok || (ok && original != tc.original) {
			t.Errorf("SplitWayback(%q) = (%q, %v), want (%q, %v)", tc.base, original, ok, tc.original, tc.ok)
		}
	}
}

func TestBuildWaybackIDURL(t *testing.T) {
	got := BuildWaybackIDURL("20220101122334", "https://discord.com/assets/x.js")
	want := "https://web.archive.org/web/20220101122334id_/https://discord.com/assets/x.js"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseCDXResponse(t *testing.T) {
	body := []byte(`[["urlkey","timestamp","original","statuscode","digest"],
["com,discord)/assets/x.js","20220601120000","https://discord.com/assets/x.js","200","abc"],
["com,discord)/assets/x.js","20220701120000","https://discord.com/assets/x.js","302","def"]]`)
	rows, err := parseCDXResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (302 filtered), got %d", len(rows))
	}
	if rows[0].Timestamp != "20220601120000" || rows[0].Original != "https://discord.com/assets/x.js" {
		t.Fatalf("row: %+v", rows[0])
	}
}

func TestParseCDXResponseEmpty(t *testing.T) {
	if _, err := parseCDXResponse([]byte(`[]`)); err == nil {
		t.Fatal("expected error for empty cdx")
	}
}
