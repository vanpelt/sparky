package netrules

// The common trusted domains: the set a person means when they say "let this
// sandbox reach the ordinary places software comes from".
//
// It is Claude Code's own published trusted-network list (code.claude.com/docs)
// plus hivemind.wandb.tools, and it is HERE, in Go, because three surfaces need
// the same answer and two of them used to hold their own copy: the console's
// "add the trusted domains" prefill, the default egress rule-set a new
// environment gets, and this package's own tests. A copy that drifts is not a
// cosmetic problem — it is a build that works in one path and cannot resolve
// npm in the other, discovered twenty minutes into a snapshot.
//
// The console's copy is JAVASCRIPT and cannot import this, so it stays a
// literal in index.html and a test compares the two sets. That test is the only
// thing keeping them equal; if you edit this list, run it.
//
// WHAT THIS IS NOT. It is not an operator's base allowlist (that is
// deploy/sluice-allowlist.txt, deliberately minimal, and it applies to every
// governed sandbox on the host) and it is not a claim that these 160-odd names
// are harmless. It is the working set of package registries, source hosts,
// language toolchains and cloud APIs that a build reaches for, chosen by the
// vendor of the agent doing the building — wide enough that an unattended agent
// does not fail on a name it had no way to anticipate, and narrow enough to
// still be a policy rather than an open door.
//
// Sorted, because it is compared as a set and read as a list, and sluice takes
// a bare name as "this domain AND its subdomains" — so `docker.io` covers
// registry-1, auth and index, and `ubuntu.com` covers the archive mirrors.
var TrustedDomains = []string{
	"*.amazonaws.com", "*.api.aws", "*.data.mcr.microsoft.com",
	"*.datadoghq.com", "*.datadoghq.eu", "*.gcr.io", "*.googleapis.com",
	"*.microsoftonline.com", "*.modelcontextprotocol.io", "*.nixos.org",
	"*.packagecloud.io", "*.sentry.io", "*.sourceforge.net", "*.ubuntu.com",
	"accounts.google.com", "anaconda.com", "anaconda.org", "apache.org",
	"api.anthropic.com", "api.bitbucket.org", "api.github.com",
	"api.honeycomb.io", "api.metacpan.org", "api.nuget.org", "api.pub.dev",
	"api.rubygems.org", "api.statsig.com", "apt.releases.hashicorp.com",
	"archive.apache.org", "archive.releases.hashicorp.com",
	"archive.ubuntu.com", "auth.docker.io", "avatars.githubusercontent.com",
	"azure.com", "binaries.prisma.sh", "bitbucket.org",
	"browser-intake-us5-datadoghq.com", "camo.githubusercontent.com",
	"cdn.cocoapods.org", "central.maven.org", "claude.ai",
	"cloud.google.com", "cocoapods.org", "code.claude.com",
	"codeload.github.com", "compute.googleapis.com", "conda.anaconda.org",
	"container.googleapis.com", "continuum.io", "cpan.org", "crates.io",
	"dev.azure.com", "developer.android.com", "developer.apple.com",
	"dl.k8s.io", "docs.claude.com", "dot.net", "dotnet.microsoft.com",
	"download.docker.com", "download.eclipse.org", "download.oracle.com",
	"downloads.apache.org", "downloads.sentry-cdn.com", "eclipse.org",
	"files.pythonhosted.org", "fonts.googleapis.com", "fonts.gstatic.com",
	"gcloud.google.com", "gcr.io", "get.rvm.io", "ghcr.io",
	"gist.github.com", "github.com", "gitlab.com", "golang.org",
	"goproxy.io", "gradle.org", "hackage.haskell.org", "hashicorp.com",
	"haskell.org", "hex.pm", "hivemind.wandb.tools",
	"http-intake.logs.datadoghq.com", "hub.docker.com", "index.crates.io",
	"index.docker.io", "index.golang.org", "index.rubygems.org", "java.com",
	"java.net", "jcenter.bintray.com", "json-schema.org",
	"json.schemastore.org", "k8s.io", "kotlinlang.org", "launchpad.net",
	"maven.org", "mcr.microsoft.com", "metacpan.org", "microsoft.com",
	"nodejs.org", "npm.pkg.github.com", "npmjs.com", "npmjs.org",
	"nuget.org", "objects.githubusercontent.com", "oracle.com",
	"packagecloud.io", "packages.microsoft.com", "packagist.org",
	"pkg-npm.githubusercontent.com", "pkg.go.dev", "pkg.stainless.com",
	"pkgs.k8s.io", "platform.claude.com", "plugins.gradle.org",
	"portal.azure.com", "ppa.launchpad.net",
	"production.cloudflare.docker.com", "proxy.golang.org", "pub.dev",
	"public.ecr.aws", "pypa.io", "pypi.org", "pypi.python.org",
	"pythonhosted.org", "registry-1.docker.io", "registry.gitlab.com",
	"registry.npmjs.org", "registry.yarnpkg.com",
	"release-assets.githubusercontent.com", "releases.hashicorp.com",
	"repo.anaconda.com", "repo.maven.apache.org", "repo.maven.org",
	"repo.packagist.org", "repo.spring.io", "repo1.maven.org",
	"rpm.releases.hashicorp.com", "ruby-lang.org", "rubyforge.org",
	"rubygems.org", "rubyonrails.org", "rustup.rs", "rvm.io",
	"security.ubuntu.com", "sentry.io", "services.gradle.org",
	"sourceforge.net", "spring.io", "static.crates.io",
	"static.rust-lang.org", "statsig.anthropic.com", "statsig.com",
	"storage.googleapis.com", "sum.golang.org", "swift.org", "test.pypi.org",
	"ubuntu.com", "visualstudio.com", "yarnpkg.com", "yum.oracle.com",
}
