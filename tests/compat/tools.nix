{ pkgs }:
pkgs.buildGoModule {
  pname = "confctl-compat-tools";
  version = "0.1.0";
  src = ./.;
  vendorHash = null;
  subPackages = [
    "cmd/compat"
    "cmd/compat-bench"
    "cmd/fixture-tool"
  ];
  checkPhase = ''
    runHook preCheck
    go test ./...
    go vet ./...
    runHook postCheck
  '';
  postInstall = ''
    mkdir -p "$out/fixtures/bin"
    for tool in git ssh nix nix-copy-closure nix-env nix-collect-garbage nix-shell; do
      ln -s "$out/bin/fixture-tool" "$out/bin/$tool"
      ln -s "$out/bin/fixture-tool" "$out/fixtures/bin/$tool"
    done
  '';
}
