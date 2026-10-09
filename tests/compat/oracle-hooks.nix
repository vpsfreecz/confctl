# Harness-only entrypoint retaining the oracle's exact Ruby/deps/src.
{
  pkgs,
  oracle,
  fixtureTools,
}:
pkgs.writeShellScriptBin "oracle-hook-driver" ''
  export GEM_HOME="${oracle.deps}/${oracle.ruby.gemPath}"
  export GEM_PATH="${oracle.deps}/${oracle.ruby.gemPath}"
  export RUBYLIB="${oracle.src}/lib"
  export PATH="${fixtureTools}/bin:$PATH"
  exec ${oracle.ruby}/bin/ruby ${./oracle_hook_driver.rb} "$@"
''
