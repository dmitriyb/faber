# Build overlay for the quickstart: the claude-code CLI as a pinned
# Anthropic native release, overriding nixpkgs' own claude-code attribute.
# The template references it as `overlay: ./nix/overlay.nix` (paths are
# read relative to the working directory, so run faber from this example's
# directory) and pins nixpkgs with a `pin:` block beside it.
#
# To bump: the version is at https://downloads.claude.ai/claude-code-releases/latest ;
# the per-platform sha256 is manifest.json .platforms[<platform>].checksum.
final: prev:

let
  arch = if prev.stdenv.hostPlatform.isAarch64 then "arm64" else "x64";
  platform = "linux-${arch}";   # dockerTools images are glibc, so no "-musl"

  claudeVersion = "2.1.220";
  claudeSha256 = {
    "linux-x64"   = "674f61f20ff306f3100cf9200e4c36c4b70278b5bef2884549819b942a89c863";
    "linux-arm64" = "159e4a51d796f3bf14677577100f7efb845611b1ceaf0c30cbd8d4650d942185";
  };
in
{
  # The release is a Bun single-file executable: the app is appended after the
  # ELF and located by an on-disk offset, so anything that shifts the layout
  # breaks it. `strip` leaves a stale offset (claude degrades to the bare Bun
  # runtime) and an added rpath pushes the ELF version tables past EOF (the
  # loader segfaults). autoPatchelfHook is safe only because glibc is the sole
  # NEEDED library: it sets the interpreter and adds no rpath. Keep buildInputs
  # empty.
  claude-code = final.stdenv.mkDerivation {
    pname = "claude-code";
    version = claudeVersion;
    src = final.fetchurl {
      url = "https://downloads.claude.ai/claude-code-releases/${claudeVersion}/${platform}/claude";
      sha256 = claudeSha256.${platform};
    };
    dontUnpack = true;
    dontStrip = true;
    nativeBuildInputs = [ final.autoPatchelfHook final.makeBinaryWrapper ];
    installPhase = ''
      runHook preInstall
      install -Dm755 "$src" "$out/bin/.claude-unwrapped"
      # --inherit-argv0 preserves claude's argv[0]-based multi-call dispatch;
      # USE_BUILTIN_RIPGREP=0 + real rg on PATH avoids the embedded fast tool.
      makeBinaryWrapper "$out/bin/.claude-unwrapped" "$out/bin/claude" \
        --inherit-argv0 \
        --set DISABLE_AUTOUPDATER 1 \
        --set DISABLE_INSTALLATION_CHECKS 1 \
        --set USE_BUILTIN_RIPGREP 0 \
        --prefix PATH : ${final.lib.makeBinPath [ final.ripgrep final.procps final.bubblewrap final.socat ]}
      runHook postInstall
    '';
    meta.description = "Claude Code native CLI (pinned Anthropic release, Bun single-file executable)";
  };
}
