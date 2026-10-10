{
  pkgs,
  fixtureTools,
  sitePackage,
  registryTemplate,
}:
let
  native = import ./package.nix {
    pkgs = pkgs // {
      git = fixtureTools;
      openssh = fixtureTools;
      nix = fixtureTools;
    };
  };
in
native.overrideAttrs (old: {
  postInstall = old.postInstall + ''
    mkdir -p "$out/share/confctl-go-prototype"
    substitute ${registryTemplate} "$out/share/confctl-go-prototype/registry.json" \
      --replace-fail '@SITE@' '${sitePackage}'
  '';
})
