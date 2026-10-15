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
  vendorHash = null;
  subPackages = [ "cmd/confctl-docs" ];
  checkPhase = ''
    runHook preCheck
    unset CONFCTL_GENERATION_RUBY CONFCTL_GENERATION_WRITER_EVIDENCE CONFCTL_DOCGEN_PANDOC
    go test ./internal/docgen ./cmd/confctl-docs -count=1
    go vet ./internal/docgen ./cmd/confctl-docs
    runHook postCheck
  '';
  meta.mainProgram = "confctl-docs";
}
