{
  pkgs,
  src ? ../.,
  moduleOptions,
  version,
  date,
}:
let
  renderer = import ./docgen.nix { inherit pkgs; };
  tool = import ./docgen-tool.nix { inherit pkgs src; };
  optionsJSON = builtins.toFile "confctl-module-options.json" (builtins.toJSON moduleOptions);
  metadataArgs = pkgs.lib.escapeShellArgs [
    "--version"
    version
    "--date"
    date
  ];
  markdown =
    pkgs.runCommand "confctl-reference-markdown"
      {
        nativeBuildInputs = [ tool ];
        LC_ALL = "C.UTF-8";
        TZ = "UTC";
        SOURCE_DATE_EPOCH = "1";
      }
      ''
        mkdir -p "$out" "$TMPDIR/home"
        export HOME="$TMPDIR/home"
        ${pkgs.lib.escapeShellArg "${tool}/bin/confctl-docs"} --kind commands ${metadataArgs} \
          --output "$out/confctl.8.md"
        ${pkgs.lib.escapeShellArg "${tool}/bin/confctl-docs"} --kind options ${metadataArgs} \
          --module-options ${pkgs.lib.escapeShellArg optionsJSON} \
          --source-root ${pkgs.lib.escapeShellArg (toString src)} \
          --output "$out/confctl-options.nix.8.md"
      '';
  renderReference =
    render: name:
    render {
      inherit name date;
      src = "${markdown}/${name}.8.md";
      footer = version;
      header = "confctl";
    };
  commandMan = renderReference renderer.renderMan "confctl";
  optionsMan = renderReference renderer.renderMan "confctl-options.nix";
  commandHTML = renderReference renderer.renderHTML "confctl";
  optionsHTML = renderReference renderer.renderHTML "confctl-options.nix";
  kexecMan = import ./kexec-netboot-man.nix { inherit pkgs; };
  kexecSource = ./modules/confctl/kexec-netboot/kexec-netboot.8.md;
  kexecHTML = renderer.renderHTML {
    name = "kexec-netboot";
    src = kexecSource;
    date = "2026-10-14";
    footer = "kexec-netboot";
    header = "kexec-netboot";
  };
  index = renderer.renderHTML {
    name = "confctl-documentation";
    src = src + "/template/docgen/index.md";
    inherit date;
    footer = version;
    header = "confctl";
  };
in
pkgs.runCommand "confctl-docs" { } ''
  mkdir -p "$out/share/man/man8" "$out/share/doc/confctl/html" "$out/share/doc/confctl/markdown"
  install -m 0644 ${commandMan} "$out/share/man/man8/confctl.8"
  install -m 0644 ${optionsMan} "$out/share/man/man8/confctl-options.nix.8"
  install -m 0644 ${kexecMan} "$out/share/man/man8/kexec-netboot.8"
  install -m 0644 ${commandHTML} "$out/share/doc/confctl/html/confctl.8.html"
  install -m 0644 ${optionsHTML} "$out/share/doc/confctl/html/confctl-options.nix.8.html"
  install -m 0644 ${kexecHTML} "$out/share/doc/confctl/html/kexec-netboot.8.html"
  install -m 0644 ${index} "$out/share/doc/confctl/html/index.html"
  install -m 0644 ${pkgs.lib.escapeShellArg "${src + "/template/docgen/style.css"}"} "$out/share/doc/confctl/html/style.css"
  install -m 0644 ${markdown}/confctl.8.md "$out/share/doc/confctl/markdown/confctl.8.md"
  install -m 0644 ${markdown}/confctl-options.nix.8.md "$out/share/doc/confctl/markdown/confctl-options.nix.8.md"
  install -m 0644 ${kexecSource} "$out/share/doc/confctl/markdown/kexec-netboot.8.md"
''
