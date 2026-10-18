//! Private implementation library for confctl and its isolated acceptance tests.
//!
//! The Ruby executable remains the product entrypoint during foundation work.

pub mod app;
pub mod cli;
pub mod domain;
pub mod error;
pub mod events;
pub mod process;
pub mod terminal;
pub mod timing;
