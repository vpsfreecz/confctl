{ pkgs }:
let
  renderer = import ../../nix/docgen.nix { inherit pkgs; };
  metadata = {
    name = "confctl-docgen-fixture";
    src = ./fixture.md;
    date = "2026-10-14";
    footer = "fixture footer";
    header = "fixture header";
  };
  man = renderer.renderMan metadata;
  html = renderer.renderHTML metadata;
  manual = import ../../nix/kexec-netboot-man.nix { inherit pkgs; };

  # Distinct derivations run the unchanged renderer in separate build dirs.
  manAgain = man.overrideAttrs (_: {
    name = "confctl-docgen-fixture-repeat.8";
  });
  htmlAgain = html.overrideAttrs (_: {
    name = "confctl-docgen-fixture-repeat.html";
  });
  manualAgain = manual.overrideAttrs (_: {
    name = "kexec-netboot-repeat.8";
  });
in
pkgs.runCommand "confctl-docgen-t1bm-check"
  {
    nativeBuildInputs = [
      pkgs.pandoc
      pkgs.python3
      pkgs.groff
    ];
    LC_ALL = "C.UTF-8";
    TZ = "UTC";
    SOURCE_DATE_EPOCH = "1";
    fixtureSrc = ./fixture.md;
    checkSource = ./check.py;
  }
  ''
    checkDir="$(mktemp -d "$TMPDIR/confctl-docgen-check.XXXXXX")"
    trap 'rm -rf "$checkDir"' EXIT
    mkdir "$checkDir/home"
    export HOME="$checkDir/home"
    cd "$checkDir"

    for file in ${
      pkgs.lib.escapeShellArgs [
        man
        html
        manual
        manAgain
        htmlAgain
        manualAgain
      ]
    }; do
      test -f "$file"
      test ! -L "$file"
      test ! -x "$file"
    done
    cmp ${pkgs.lib.escapeShellArg man} ${pkgs.lib.escapeShellArg manAgain}
    cmp ${pkgs.lib.escapeShellArg html} ${pkgs.lib.escapeShellArg htmlAgain}
    cmp ${pkgs.lib.escapeShellArg manual} ${pkgs.lib.escapeShellArg manualAgain}

    ${pkgs.lib.escapeShellArg "${pkgs.pandoc}/bin/pandoc"} \
      --from=markdown+definition_lists+fenced_code_blocks+backtick_code_blocks+pipe_tables+autolink_bare_uris+strikeout+superscript+yaml_metadata_block-raw_html-raw_tex \
      --to=json --output=fixture.json "$fixtureSrc"
    ${pkgs.lib.escapeShellArg "${pkgs.groff}/bin/groff"} -Kutf8 -t -Tutf8 -man -P-b -P-u \
      ${pkgs.lib.escapeShellArg man} > fixture-visible.txt
    ${pkgs.lib.escapeShellArg "${pkgs.groff}/bin/groff"} -Kutf8 -t -Tutf8 -man -P-b -P-u \
      ${pkgs.lib.escapeShellArg manual} > manual-visible.txt
    ${pkgs.lib.escapeShellArg "${pkgs.python3}/bin/python3"} "$checkSource" \
      fixture.json ${pkgs.lib.escapeShellArg man} ${pkgs.lib.escapeShellArg html} \
      fixture-visible.txt ${pkgs.lib.escapeShellArg manual} manual-visible.txt
    touch "$out"
  ''
