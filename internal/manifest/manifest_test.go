package manifest

import "testing"

func TestIsExternalSecret(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			name: "external secret v1beta1",
			data: "apiVersion: external-secrets.io/v1beta1\nkind: ExternalSecret\nmetadata:\n  name: db\n",
			want: true,
		},
		{
			name: "external secret v1",
			data: "apiVersion: external-secrets.io/v1\nkind: ExternalSecret\n",
			want: true,
		},
		{
			name: "configmap is not an external secret",
			data: "apiVersion: v1\nkind: ConfigMap\ndata:\n  a: b\n",
			want: false,
		},
		{
			name: "right kind wrong group",
			data: "apiVersion: example.com/v1\nkind: ExternalSecret\n",
			want: false,
		},
		{
			name: "right group wrong kind",
			data: "apiVersion: external-secrets.io/v1beta1\nkind: SecretStore\n",
			want: false,
		},
		{
			name: "multi-doc with external secret second",
			data: "apiVersion: v1\nkind: ConfigMap\n---\napiVersion: external-secrets.io/v1\nkind: ExternalSecret\n",
			want: true,
		},
		{
			name: "plain config not yaml mapping",
			data: "log.level=debug\nlog.format=json\n",
			want: false,
		},
		{
			name: "empty",
			data: "",
			want: false,
		},
		{
			name: "json external secret",
			data: `{"apiVersion":"external-secrets.io/v1beta1","kind":"ExternalSecret"}`,
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsExternalSecret([]byte(tc.data)); got != tc.want {
				t.Fatalf("IsExternalSecret() = %v, want %v", got, tc.want)
			}
		})
	}
}
