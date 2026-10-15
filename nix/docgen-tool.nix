{
  pkgs,
  src ? ../.,
}:

pkgs.buildGoModule {
  pname = "confctl-docgen";
  version = "dev";
  # Keep the imported core/cli/docgen/extension packages, tests, fixtures and
  # embedded context. This tool is not the standalone carrier command.
  src = pkgs.lib.cleanSource src;
  vendorHash = (import ./go-dependency-hashes.nix { inherit (pkgs) lib; }).vendorHash;
  subPackages = [ "cmd/confctl-docs" ];
  checkPhase = ''
    runHook preCheck
    unset CONFCTL_GENERATION_RUBY CONFCTL_GENERATION_WRITER_EVIDENCE CONFCTL_DOCGEN_PANDOC
    go test ./internal/docgen ./cmd/confctl-docs -count=1
    go vet ./internal/docgen ./cmd/confctl-docs
    runHook postCheck
  '';
  postInstall = ''
    mkdir -p "$out/share/doc/confctl-docgen/notices/dependencies"
    cp -R licenses/go/. "$out/share/doc/confctl-docgen/notices/dependencies/"
    install -Dm444 LICENSE.txt "$out/share/doc/confctl-docgen/LICENSE.txt"
  '';
  meta.mainProgram = "confctl-docs";
}
