#![allow(macro_expanded_macro_exports_accessed_by_absolute_paths)]

#[macro_use]
extern crate rocket;

use std::path;

use protobuf::Message;
use rocket::serde::json::{json, Json, Value as JsonValue};
use serde::Deserialize;
use syntax_analysis::highlighting::{ScipHighlightQuery, SourcegraphQuery};
use tree_sitter_all_languages::ParserId;

#[post("/", format = "application/json", data = "<q>")]
fn syntect(q: Json<SourcegraphQuery>) -> JsonValue {
    // TODO(slimsag): In an ideal world we wouldn't be relying on catch_unwind
    // and instead Syntect would return Result types when failures occur. This
    // will require some non-trivial work upstream:
    // https://github.com/trishume/syntect/issues/98
    let result = std::panic::catch_unwind(|| {
        syntax_analysis::highlighting::syntect_highlight(q.into_inner())
    });
    match result {
        Ok(v) => v,
        Err(_) => json!({"error": "panic while highlighting code", "code": "panic"}),
    }
}

// TODO: Once we're confident we don't need this anymore, we can remove this entirely
// and just have the `scip` endpoint. But I figured I would make it available at least
// for now, since I'm working on doing that.
#[post("/lsif", format = "application/json", data = "<q>")]
fn lsif(q: Json<SourcegraphQuery>) -> JsonValue {
    match syntax_analysis::highlighting::lsif_highlight(q.into_inner()) {
        Ok(v) => v,
        Err(err) => err,
    }
}

#[post("/scip", format = "application/json", data = "<q>")]
fn scip(q: Json<ScipHighlightQuery>) -> JsonValue {
    match syntax_analysis::highlighting::scip_highlight(q.into_inner()) {
        Ok(v) => v,
        Err(err) => err,
    }
}

#[derive(Deserialize, Default, Debug)]
pub struct SymbolQuery {
    filename: String,
    content: String,
}

pub fn jsonify_err(e: impl ToString) -> JsonValue {
    json!({"error": e.to_string()})
}

#[post("/symbols", format = "application/json", data = "<q>")]
fn symbols(q: Json<SymbolQuery>) -> JsonValue {
    let path = path::Path::new(&q.filename);
    let extension = match match path.extension() {
        Some(vals) => vals,
        None => {
            return json!({"error": "Extensionless file"});
        }
    }
    .to_str()
    {
        Some(vals) => vals,
        None => {
            return json!({"error": "Invalid codepoint"});
        }
    };
    let parser = match ParserId::from_file_extension(extension) {
        Some(parser) => parser,
        None => return json!({"error": "Could not infer parser from extension"}),
    };

    let (mut scope, hint) = match syntax_analysis::get_globals(parser, q.content.as_bytes()) {
        Some(Ok(vals)) => vals,
        Some(Err(err)) => return jsonify_err(err),
        None => return json!({"error": "Failed to get globals"}),
    };

    let document = scope.into_document(hint, vec![]);

    let encoded = match document.write_to_bytes() {
        Ok(vals) => vals,
        Err(err) => {
            return jsonify_err(err);
        }
    };
    json!({"scip": base64::encode(encoded), "plaintext": false})
}

#[get("/health")]
fn health() -> &'static str {
    "OK"
}

#[catch(404)]
fn not_found() -> JsonValue {
    json!({"error": "resource not found", "code": "resource_not_found"})
}

#[launch]
fn rocket() -> _ {
    // Exits with a code zero if the environment variable SANITY_CHECK equals
    // to "true". This enables testing that the current program is in a runnable
    // state against the platform it's being executed on.
    //
    // See https://github.com/GoogleContainerTools/container-structure-test
    match std::env::var("SANITY_CHECK") {
        Ok(v) if v == "true" => {
            println!("Sanity check passed, exiting without error");
            std::process::exit(0)
        }
        _ => {}
    };

    // load configurations on-startup instead of on-first-request.
    // TODO: load individual languages lazily on-request instead, currently
    // CONFIGURATIONS.get will load every configured configuration together.
    syntax_analysis::highlighting::tree_sitter::CONFIGURATIONS
        .get(&tree_sitter_all_languages::ParserId::Go);

    // Only list features if QUIET != "true"
    match std::env::var("QUIET") {
        Ok(v) if v == "true" => {}
        _ => syntax_analysis::highlighting::list_features(),
    };

    rocket::build()
        .mount("/", routes![syntect, lsif, scip, symbols, health])
        .register("/", catchers![not_found])
}
