import Foundation

/// Loose JSON value for tool-call arguments and other free-form payloads.
enum JSONValue: Codable, Equatable, Sendable {
    case string(String)
    case number(Double)
    case bool(Bool)
    case object([String: JSONValue])
    case array([JSONValue])
    case null

    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null; return }
        if let b = try? c.decode(Bool.self) { self = .bool(b); return }
        if let n = try? c.decode(Double.self) { self = .number(n); return }
        if let s = try? c.decode(String.self) { self = .string(s); return }
        if let a = try? c.decode([JSONValue].self) { self = .array(a); return }
        self = .object(try c.decode([String: JSONValue].self))
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .string(let s): try c.encode(s)
        case .number(let n): try c.encode(n)
        case .bool(let b): try c.encode(b)
        case .object(let o): try c.encode(o)
        case .array(let a): try c.encode(a)
        case .null: try c.encodeNil()
        }
    }

    /// Pretty-printed JSON text for display.
    var pretty: String {
        switch self {
        case .string(let s):
            return "\"\(s)\""
        case .number(let n):
            return n.truncatingRemainder(dividingBy: 1) == 0 ? String(Int(n)) : String(n)
        case .bool(let b): return b ? "true" : "false"
        case .null: return "null"
        case .array(let a):
            if a.isEmpty { return "[]" }
            let inner = a.map { $0.pretty.indented() }.joined(separator: ",\n")
            return "[\n\(inner)\n]"
        case .object(let o):
            if o.isEmpty { return "{}" }
            let inner = o.keys.sorted().map { k in
                "\"\(k)\": \(o[k]!.pretty)".indented()
            }.joined(separator: ",\n")
            return "{\n\(inner)\n}"
        }
    }

    /// Compact one-line summary for collapsed rows.
    var summary: String {
        switch self {
        case .string(let s): return s.count > 60 ? String(s.prefix(57)) + "…" : s
        case .object(let o):
            return o.keys.sorted().prefix(3).map { "\($0): \(o[$0]!.summary)" }
                .joined(separator: ", ")
        case .array(let a): return "[\(a.count) items]"
        default: return pretty.replacingOccurrences(of: "\n", with: " ")
        }
    }
}

private extension String {
    func indented() -> String {
        split(separator: "\n", omittingEmptySubsequences: false)
            .map { "  " + $0 }.joined(separator: "\n")
    }
}
