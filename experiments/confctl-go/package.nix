{ pkgs }:
pkgs.buildGoModule {
  pname = "confctl-go-prototype";
  version = "0.1.0";
  src = ../..;
  vendorHash = null;
  subPackages = [
    "cmd/confctl-go-prototype"
    "cmd/hook-driver"
  ];
  nativeBuildInputs = [ pkgs.makeWrapper ];
  checkPhase = ''
    runHook preCheck
    go test ./...
    go vet ./...
    runHook postCheck
  '';
  postInstall = ''
    install -Dm444 internal/inputs/ordered_lock.NOTICE \
      "$out/share/doc/confctl-go-prototype/ordered_lock.NOTICE"
    install -Dm444 internal/core/process_log_source.NOTICE \
      "$out/share/doc/confctl-go-prototype/process_log_source.NOTICE"
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
  '';
}
