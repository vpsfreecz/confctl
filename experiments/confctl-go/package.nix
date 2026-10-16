{ pkgs }:
pkgs.buildGoModule {
  pname = "confctl-go-prototype";
  version = "0.1.0";
  src = ../..;
  vendorHash = (import ../../nix/go-dependency-hashes.nix { inherit (pkgs) lib; }).vendorHash;
  subPackages = [
    "cmd/confctl-go-prototype"
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
    go test ./...
    go vet ./...
    runHook postCheck
  '';
  postInstall = ''
    mkdir -p "$out/share/doc/confctl-go-prototype/dependencies"
    cp -R licenses/go/. "$out/share/doc/confctl-go-prototype/dependencies/"
    install -Dm444 internal/inputs/ordered_lock.NOTICE \
      "$out/share/doc/confctl-go-prototype/ordered_lock.NOTICE"
    install -Dm444 internal/core/process_log_source.NOTICE \
      "$out/share/doc/confctl-go-prototype/process_log_source.NOTICE"
    install -Dm444 internal/ui/pager_source.NOTICE \
      "$out/share/doc/confctl-go-prototype/pager_source.NOTICE"
    install -Dm444 internal/core/gendata_prompt_linux.NOTICE \
      "$out/share/doc/confctl-go-prototype/gendata_prompt_linux.NOTICE"
    install -Dm444 internal/ui/gendata_prompt.NOTICE \
      "$out/share/doc/confctl-go-prototype/gendata_prompt.NOTICE"
    mkdir -p "$out/libexec"
    substitute libexec/confctl-commit-stop-relay.bash \
      "$out/libexec/confctl-commit-stop-relay.bash" \
      --replace-fail '@BASH@' '${pkgs.bash}/bin/bash'
    chmod 755 "$out/libexec/confctl-commit-stop-relay.bash"
    for bin in confctl-go-prototype hook-driver; do
      wrapProgram "$out/bin/$bin" --prefix PATH : ${
        pkgs.lib.makeBinPath [
          pkgs.git
          pkgs.openssh
          pkgs.nix
          pkgs.glibc.bin
        ]
      }
    done
    install -Dm444 LICENSE.txt "$out/share/doc/confctl-go-prototype/LICENSE.txt"
  '';
}
