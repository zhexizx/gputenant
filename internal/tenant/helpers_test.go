package tenant

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

func TestPickPort(t *testing.T) {
	allFree := func(int) bool { return true }
	cases := []struct {
		name  string
		taken map[int]bool
		free  func(int) bool
		want  int
	}{
		{"empty", nil, allFree, 16443},
		{"first taken", map[int]bool{16443: true}, allFree, 16444},
		{"gap reused", map[int]bool{16444: true}, allFree, 16443},
		{"other process", map[int]bool{16443: true}, func(p int) bool { return p != 16444 }, 16445},
		{"none free", nil, func(int) bool { return false }, 0},
	}
	for _, c := range cases {
		if got := pickPort(c.taken, 16443, c.free); got != c.want {
			t.Errorf("%s: pickPort = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestParseHostPort(t *testing.T) {
	if got := parseHostPort(`{"6443/tcp":[{"HostIp":"127.0.0.1","HostPort":"16444"}]}`); got != 16444 {
		t.Errorf("got %d, want 16444", got)
	}
	for _, in := range []string{`{}`, `null`, `{"80/tcp":[{"HostPort":"80"}]}`, `garbage`} {
		if got := parseHostPort(in); got != 0 {
			t.Errorf("parseHostPort(%s) = %d, want 0", in, got)
		}
	}
}

const sampleKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: tenant-a
  cluster:
    server: https://172.18.255.200:6443
    certificate-authority-data: Y2E=
contexts:
- name: admin@tenant-a
  context: {cluster: tenant-a, user: admin}
current-context: admin@tenant-a
users:
- name: admin
  user: {token: abc}
`

func TestRewriteKubeconfig(t *testing.T) {
	out, err := rewriteKubeconfig([]byte(sampleKubeconfig), 16444, "172.18.255.200")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Clusters["tenant-a"]
	if c.Server != "https://127.0.0.1:16444" || c.TLSServerName != "172.18.255.200" {
		t.Fatalf("server=%s tls=%s", c.Server, c.TLSServerName)
	}
	if string(c.CertificateAuthorityData) != "ca" || cfg.AuthInfos["admin"].Token != "abc" {
		t.Fatal("credentials not preserved")
	}
	// 同样的输入必须得到同样的输出，Check 靠字节比较判断是否已完成。
	again, _ := rewriteKubeconfig([]byte(sampleKubeconfig), 16444, "172.18.255.200")
	if string(again) != string(out) {
		t.Fatal("rewriteKubeconfig is not deterministic")
	}
}

func TestPatchCalicoCIDR(t *testing.T) {
	commented := "        env:\n            # - name: CALICO_IPV4POOL_CIDR\n            #   value: \"192.168.0.0/16\"\n            - name: X\n"
	patched := "        env:\n            - name: CALICO_IPV4POOL_CIDR\n              value: \"10.244.0.0/16\"\n            - name: X\n"
	for name, in := range map[string]string{"commented": commented, "already patched": patched,
		"other value": strings.Replace(patched, "10.244.0.0/16", "10.0.0.0/8", 1)} {
		out, err := patchCalicoCIDR([]byte(in), "10.244.0.0/16")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(out) != patched {
			t.Errorf("%s:\n got %q\nwant %q", name, out, patched)
		}
	}
	if _, err := patchCalicoCIDR([]byte("no such entry"), "10.244.0.0/16"); err == nil {
		t.Error("want error when entry is missing")
	}
}

func TestRenderKubeProxy(t *testing.T) {
	out := renderKubeProxy(kubeProxyManifest, "172.18.255.200")
	if strings.Contains(out, "${") {
		t.Fatalf("unreplaced placeholder left in kube-proxy manifest")
	}
	for _, want := range []string{"kube-proxy:" + K8sVersion, "--cluster-cidr=" + PodCIDR, `"172.18.255.200"`, "--conntrack-max-per-core=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered kube-proxy missing %q", want)
		}
	}
	objs, err := DecodeYAML([]byte(out))
	if err != nil || len(objs) != 3 {
		t.Fatalf("want 3 objects, got %d (%v)", len(objs), err)
	}
}

func TestEmbeddedManifestsDecode(t *testing.T) {
	for name, data := range map[string][]byte{
		"require-hami-scheduler": requireHamiScheduler, "vap-gate-pod": vapGatePod, "prometheus-rbac": prometheusRBAC,
	} {
		objs, err := DecodeYAML(data)
		if err != nil || len(objs) == 0 {
			t.Errorf("%s: %d objects, err %v", name, len(objs), err)
		}
	}
}

func TestCheckHAMiRender(t *testing.T) {
	good := `
        image: "hami:local"
      - image: registry.k8s.io/kube-scheduler:` + K8sVersion + `
        image: docker.io/jettech/kube-webhook-certgen:v1.5.2
        - --driver-root=/var/lib/nvml-mock/driver`
	if err := checkHAMiRender(good); err != nil {
		t.Fatalf("good render rejected: %v", err)
	}
	bad := map[string]string{
		"upstream image":  good + "\n        image: projecthami/hami:v2",
		"no local image":  strings.Replace(good, "hami:local", "other:local", 1),
		"wrong scheduler": strings.Replace(good, K8sVersion, "v1.30.0", 1),
		"no driver root":  strings.Replace(good, "/var/lib/nvml-mock/driver", "/", 1),
	}
	for name, r := range bad {
		if err := checkHAMiRender(r); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestYAMLScalar(t *testing.T) {
	doc := "apiVersion: kubelet.config.k8s.io/v1beta1\ncgroupDriver: systemd\nfailSwapOn: false\n"
	if got := yamlScalar(doc, "failSwapOn"); got != "false" {
		t.Errorf("got %q", got)
	}
	if got := yamlScalar(doc, "missing"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"tenant-a", "t1", "a"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Tenant", "-a", "a-", "a_b", "a.b", strings.Repeat("a", 33)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestVersionsMatchLibSh：bash 和 Go 各存一份版本号，这里防止两边漂移。
func TestVersionsMatchLibSh(t *testing.T) {
	data, err := os.ReadFile("../../scripts/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^([A-Z0-9_]+)=("?)([^"\n]*)("?)$`).FindAllStringSubmatch(string(data), -1) {
		vars[m[1]] = m[3]
	}
	for shName, goVal := range map[string]string{
		"K8S_VERSION": K8sVersion, "NODE_IMAGE": NodeImage, "POD_CIDR": PodCIDR, "SOCAT_IMAGE": SocatImage,
		"CALICO_VERSION": CalicoVersion, "NVML_MOCK_CHART_VERSION": NvmlMockChartVersion,
		"PROMETHEUS_CHART_REPO": PrometheusChartRepo, "PROMETHEUS_CHART_VERSION": PrometheusChartVer,
	} {
		if vars[shName] != goVal {
			t.Errorf("%s: lib.sh has %q, Go has %q", shName, vars[shName], goVal)
		}
	}
}

// Dockerfile 里的 HAMi commit 和 helm 版本要和 lib.sh 一致，否则镜像里装的 HAMi 和本地测过的不是同一个。
func TestDockerfileMatchesLibSh(t *testing.T) {
	lib, err := os.ReadFile("../../scripts/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	df, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	get := func(data []byte, re string) string {
		m := regexp.MustCompile(re).FindSubmatch(data)
		if m == nil {
			t.Fatalf("pattern %s not found", re)
		}
		return string(m[1])
	}
	if a, b := get(lib, `(?m)^HAMI_COMMIT=(\S+)$`), get(df, `(?m)^ARG HAMI_COMMIT=(\S+)$`); a != b {
		t.Errorf("HAMI_COMMIT: lib.sh %s, Dockerfile %s", a, b)
	}
	if a, b := get(lib, `(?m)^HELM_VERSION=v(\S+)$`), get(df, `(?m)^FROM alpine/helm:(\S+) AS helm$`); a != b {
		t.Errorf("helm: lib.sh v%s, Dockerfile %s", a, b)
	}
	for _, wf := range []string{"release.yml", "e2e.yml"} {
		data := mustRead(t, "../../.github/workflows/"+wf)
		for _, v := range []string{"HAMI_COMMIT", "K8S_TEST_INFRA_COMMIT"} {
			if a, b := get(lib, `(?m)^`+v+`=(\S+)$`), get(data, v+`: (\S+)`); a != b {
				t.Errorf("%s: lib.sh %s, %s %s", v, a, wf, b)
			}
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
