package migrate

import "testing"

func TestAddActionlintLabels(t *testing.T) {
	cases := []struct {
		in, want string
		changed  bool
	}{
		{
			in:      "# comment\nself-hosted-runner:\n  labels:\n    - general\n",
			want:    "# comment\nself-hosted-runner:\n  labels:\n    - general\n    - my-ci-linux\n    - my-ci-windows\n    - my-ci\n",
			changed: true,
		},
		{
			in:      "self-hosted-runner:\n  labels:\n    - general\n    - my-ci-linux\n    - my-ci-windows\n    - my-ci\nconfig-variables: null\n",
			changed: false,
		},
		{
			in:      "self-hosted-runner:\n  labels:\n    - general\n\nconfig-variables: null\n",
			want:    "self-hosted-runner:\n  labels:\n    - general\n    - my-ci-linux\n    - my-ci-windows\n    - my-ci\n\nconfig-variables: null\n",
			changed: true,
		},
		{in: "paths: {}\n", changed: false},
	}
	for i, c := range cases {
		out, ok := AddActionlintLabels([]byte(c.in), actionlintLabels("my-ci"))
		if ok != c.changed {
			t.Fatalf("case %d: changed=%v", i, ok)
		}
		if ok && string(out) != c.want {
			t.Fatalf("case %d:\n%s", i, out)
		}
	}
}
