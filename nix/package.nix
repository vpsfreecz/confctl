{
  pkgs,
  src ? ../.,
  docs ? null,
}:
pkgs.buildGoModule {
  pname = "confctl";
  version = "dev";
  src = pkgs.lib.cleanSource src;
  vendorHash = null;
  subPackages = [
    "cmd/confctl"
    "cmd/hook-driver"
  ];
  nativeBuildInputs = [ pkgs.makeWrapper ];
  nativeCheckInputs = [
    pkgs.git
    pkgs.bash
    pkgs.coreutils
    pkgs.nix
  ];
  ldflags = [
    "-X github.com/vpsfreecz/confctl/internal/core.comparisonRuntimeShell=${pkgs.runtimeShell}"
  ];
  checkPhase = ''
    runHook preCheck
    unset CONFCTL_GENERATION_RUBY CONFCTL_GENERATION_WRITER_EVIDENCE CONFCTL_DOCGEN_PANDOC
    go test ./...
    go vet ./...
    runHook postCheck
  '';
  postInstall = ''
    for notice in \
      internal/inputs/ordered_lock.NOTICE \
      internal/core/process_log_source.NOTICE \
      internal/core/gendata_prompt_linux.NOTICE \
      internal/ui/pager_source.NOTICE \
      internal/ui/gendata_prompt.NOTICE; do
      install -Dm444 "$notice" "$out/share/doc/confctl/notices/$notice"
    done
    mkdir -p "$out/libexec"
    substitute libexec/confctl-commit-stop-relay.bash \
      "$out/libexec/confctl-commit-stop-relay.bash" \
      --replace-fail '@BASH@' '${pkgs.bash}/bin/bash'
    chmod 755 "$out/libexec/confctl-commit-stop-relay.bash"
    for bin in confctl hook-driver; do
      wrapProgram "$out/bin/$bin" --prefix PATH : ${
        pkgs.lib.makeBinPath [
          pkgs.bash
          pkgs.coreutils
          pkgs.git
          pkgs.openssh
          pkgs.nix
          pkgs.glibc.bin
        ]
      }
    done
  ''
  + pkgs.lib.optionalString (docs != null) ''
    cp -R ${docs}/share/. "$out/share/"
  '';
  meta.mainProgram = "confctl";
}
