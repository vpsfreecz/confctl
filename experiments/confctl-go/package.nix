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
  nativeCheckInputs = [
    pkgs.git
    pkgs.bash
    pkgs.coreutils
    pkgs.nix
  ];
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
    install -Dm444 LICENSE.txt "$out/share/doc/confctl-go-prototype/LICENSE.txt"
  '';
}
