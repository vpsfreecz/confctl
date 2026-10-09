{ pkgs }:
pkgs.buildGoModule {
  pname = "confctl-go-prototype";
  version = "0.1.0";
  src = ./.;
  vendorHash = null;
  subPackages = [
    "cmd/confctl-go-prototype"
    "cmd/vpsfree-confctl-ext"
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
    for bin in confctl-go-prototype hook-driver; do
      wrapProgram "$out/bin/$bin" --prefix PATH : ${
        pkgs.lib.makeBinPath [
          pkgs.git
          pkgs.openssh
          pkgs.nix
        ]
      }
    done
    mkdir -p "$out/share/confctl-go-prototype"
    substitute ${./registry.json} "$out/share/confctl-go-prototype/registry.json" \
      --replace-fail '@SITE@' "$out"
  '';
}
