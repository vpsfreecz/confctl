{
  pkgs,
  checker,
  goModules ? [ ],
}:
let
  goTools = import ./hook-go-tools.nix { inherit pkgs goModules; };
  program = "${checker}/bin/confctl-hooks";
  policy = pkgs.writeText "confctl-lefthook.yml" (
    import ./lefthook-config.nix { checker = program; }
  );
  tool = output: executable: {
    Output = toString output;
    Program = "${output}/bin/${executable}";
  };
  bindings = pkgs.writeText "confctl-hook-tools.json" (
    builtins.toJSON {
      Checker = tool checker "confctl-hooks";
      Git = tool pkgs.git "git";
      Lefthook = tool pkgs.lefthook "lefthook";
      Nixfmt = tool pkgs.nixfmt "nixfmt";
      Gofmt = tool goTools "gofmt";
      Shell = tool pkgs.bash "bash";
      Path = map (p: "${p}/bin") [
        checker
        pkgs.git
        pkgs.nixfmt
        goTools
        pkgs.go
        pkgs.stdenv.cc
        pkgs.bash
        pkgs.coreutils
        pkgs.findutils
      ];
      Launcher = "@LAUNCHER@";
    }
  );
in
pkgs.runCommand "confctl-dev-tools"
  {
    nativeBuildInputs = [ pkgs.makeWrapper ];
    passthru = { inherit checker policy goTools; };
    meta.mainProgram = "confctl-hooks";
  }
  ''
    mkdir -p "$out/bin" "$out/share/confctl-hooks"
    install -m0644 ${policy} "$out/share/confctl-hooks/lefthook.yml"
    substitute ${bindings} "$out/share/confctl-hooks/tools.json" \
      --replace-fail '@LAUNCHER@' "$out/bin/confctl-hooks"
    makeWrapper ${program} "$out/bin/confctl-hooks" \
      --add-flags "--tools $out/share/confctl-hooks/tools.json"
    ln -s ${pkgs.lefthook}/bin/lefthook "$out/bin/lefthook"
    ln -s ${pkgs.nixfmt}/bin/nixfmt "$out/bin/nixfmt"
  ''
