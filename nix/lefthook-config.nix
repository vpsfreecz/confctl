{ checker }:
''
  pre-commit:
    commands:
      confctl:
        run: "${checker} check-prepared pre-commit"
  commit-msg:
    commands:
      confctl:
        run: "${checker} check-prepared commit-msg"
''
