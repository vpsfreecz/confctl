fn main() -> std::process::ExitCode {
    let timing = confctl::timing::Timing::start();
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(error) => {
            eprintln!("error: initialize Rust runtime: {error}");
            return std::process::ExitCode::FAILURE;
        }
    };
    let code = runtime.block_on(confctl::app::run(
        std::env::args_os().skip(1).collect(),
        timing,
    ));
    std::process::ExitCode::from(u8::try_from(code).unwrap_or(1))
}
