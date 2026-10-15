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
      glibc = pkgs.glibc // {
        bin = fixtureTools;
      };
    };
  };
in
native.overrideAttrs (old: {
  nativeCheckInputs = [
    pkgs.git
    pkgs.bash
    pkgs.coreutils
    pkgs.nix
  ];
  postInstall = old.postInstall + ''
    mkdir -p "$out/share/confctl-go-prototype"
    substitute ${registryTemplate} "$out/share/confctl-go-prototype/registry.json" \
      --replace-fail '@SITE@' '${sitePackage}'
  '';
})
