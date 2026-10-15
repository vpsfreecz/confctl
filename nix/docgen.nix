{ pkgs }:
let
  format = "markdown+definition_lists+fenced_code_blocks+backtick_code_blocks+pipe_tables+autolink_bare_uris+strikeout+superscript+yaml_metadata_block-raw_html-raw_tex";

  render =
    target:
    {
      name,
      src,
      date,
      footer,
      header,
    }:
    let
      args = [
        "--from=${format}"
        "--to=${target}"
        "--standalone"
        "--wrap=none"
      ]
      ++ pkgs.lib.optionals (target == "html5") [
        "--shift-heading-level-by=1"
        "--no-highlight"
        "--css=style.css"
      ]
      ++ [ "--metadata=title:${name}" ]
      ++ pkgs.lib.optionals (target == "html5") [ "--metadata=pagetitle:${name}" ]
      ++ [
        "--metadata=section:8"
        "--metadata=date:${date}"
        "--metadata=footer:${footer}"
        "--metadata=header:${header}"
      ];
    in
    pkgs.runCommand (name + (if target == "man" then ".8" else ".html"))
      {
        inherit src;
        nativeBuildInputs = [ pkgs.pandoc ];
        LC_ALL = "C.UTF-8";
        TZ = "UTC";
        SOURCE_DATE_EPOCH = "1";
      }
      ''
        renderDir="$(mktemp -d "$TMPDIR/confctl-docgen.XXXXXX")"
        trap 'rm -rf "$renderDir"' EXIT
        mkdir "$renderDir/home"
        export HOME="$renderDir/home"
        cd "$renderDir"
        ${pkgs.lib.escapeShellArg "${pkgs.pandoc}/bin/pandoc"} ${pkgs.lib.escapeShellArgs args} \
          --output="$out" "$src"
        test -f "$out"
        test ! -L "$out"
      '';
in
{
  renderMan = render "man";
  renderHTML = render "html5";
}
