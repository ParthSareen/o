import SwiftUI

/// Lightweight per-line syntax coloring for the diff view. Not a real
/// parser — strings/comments/keywords/numbers/types only — but that's all
/// the visual pop a review pane needs, and it costs microseconds per line.
enum DiffSyntax {
    private struct Lang {
        var keywords: Set<String>
        var lineComments: [String]
        var hashComments = false
    }

    private static let cLike: Set<String> = [
        "func","fn","let","var","const","return","if","else","elif","for","while","loop",
        "switch","case","default","break","continue","class","struct","enum","interface",
        "protocol","extension","impl","trait","import","package","pub","public","private",
        "internal","protected","static","final","override","new","nil","null","none","true",
        "false","self","this","Self","defer","go","goroutine","chan","select","async","await",
        "try","catch","throw","throws","rethrows","in","guard","where","typealias","mod","use",
        "mut","ref","unsafe","type","map","range","fallthrough","delete","typeof","instanceof",
        "void","int","string","bool","error","any","byte","rune","float64","int64","uint32",
    ]
    private static let python: Set<String> = [
        "def","class","return","import","from","as","if","elif","else","for","while","break",
        "continue","pass","raise","try","except","finally","with","lambda","yield","global",
        "nonlocal","assert","del","in","is","not","and","or","None","True","False","async","await",
        "print","self","cls","__init__",
    ]
    private static let shell: Set<String> = [
        "if","fi","then","else","elif","case","esac","for","in","do","done","while","until",
        "echo","cd","export","local","function","return","exit","set","source","shift","trap",
    ]

    private static func lang(for path: String) -> Lang {
        switch (path as NSString).pathExtension.lowercased() {
        case "py", "pyi": return Lang(keywords: python, lineComments: [], hashComments: true)
        case "sh", "bash", "zsh": return Lang(keywords: shell, lineComments: [], hashComments: true)
        case "json", "yaml", "yml", "toml": return Lang(keywords: ["true","false","null"], lineComments: [], hashComments: true)
        default: return Lang(keywords: cLike, lineComments: ["//"])
        }
    }

    private static let stringRegex = try! NSRegularExpression(
        pattern: #""(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|`[^`\n]*`"#)
    private static let numberRegex = try! NSRegularExpression(pattern: #"\b\d+(?:\.\d+)?(?:[eExX][\da-fA-F]+)?\b"#)
    private static let wordRegex = try! NSRegularExpression(pattern: #"\b[A-Za-z_][A-Za-z0-9_]*\b"#)

    static func highlight(
        _ line: String, path: String,
        font: Font = .system(.caption, design: .monospaced)
    ) -> AttributedString {
        let lang = lang(for: path)
        let ns = line as NSString
        let full = NSRange(location: 0, length: ns.length)
        var result = AttributedString(line)

        // 1. comments: first // or # that is not inside a string
        var commentStart: Int? = nil
        let stringRanges = stringRegex.matches(in: line, range: full).map(\.range)
        func inString(_ i: Int) -> Bool { stringRanges.contains { NSLocationInRange(i, $0) } }
        for marker in lang.lineComments {
            var idx = line.startIndex
            while let found = line.range(of: marker, range: idx..<line.endIndex) {
                let offset = line.distance(from: line.startIndex, to: found.lowerBound)
                if !inString(offset) { commentStart = min(commentStart ?? .max, offset); break }
                idx = found.upperBound
            }
        }
        if lang.hashComments {
            // first # at start-of-line or after whitespace, not inside a string
            var idx = line.startIndex
            while let found = line[idx...].firstIndex(of: "#") {
                let offset = line.distance(from: line.startIndex, to: found)
                let atBoundary = found == line.startIndex || line[line.index(before: found)] == " "
                if atBoundary && !inString(offset) { commentStart = min(commentStart ?? .max, offset); break }
                idx = line.index(after: found)
            }
        }
        let codeEnd = commentStart ?? ns.length

        if let cs = commentStart, cs < ns.length,
           let range = Range(NSRange(location: cs, length: ns.length - cs), in: result) {
            result[range].foregroundColor = .secondary
            result[range].font = font.italic()
        }

        // 2. strings
        for r in stringRanges where r.location < codeEnd {
            if let range = Range(r, in: result) {
                result[range].foregroundColor = Color(nsColor: .systemTeal)
            }
        }

        // 3. numbers
        for r in numberRegex.matches(in: line, range: NSRange(location: 0, length: codeEnd)).map(\.range) {
            if let range = Range(r, in: result) {
                result[range].foregroundColor = Color(nsColor: .systemOrange)
            }
        }

        // 4. keywords + capitalized types
        for match in wordRegex.matches(in: line, range: NSRange(location: 0, length: codeEnd)) {
            let word = ns.substring(with: match.range)
            guard let range = Range(match.range, in: result) else { continue }
            if lang.keywords.contains(word) {
                result[range].foregroundColor = Color(nsColor: .systemPurple)
            } else if word.first?.isUppercase == true && word.count > 1 {
                result[range].foregroundColor = Color(nsColor: .systemPink)
            }
        }

        return result
    }
}
