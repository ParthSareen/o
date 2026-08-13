// MARK: model

enum MdBlock: Equatable {
    case heading(level: Int, text: String)
    case bullet(text: String, depth: Int)
    case numbered(n: Int, text: String)
    case quote(String)
    case paragraph(String)
    case code(language: String, String)
    case table(headers: [String], rows: [[String]])
    case rule
}

// MARK: parser

enum MarkdownParser {
    static func blocks(_ text: String) -> [MdBlock] {
        var out: [MdBlock] = []
        var prose: [String] = []   // paragraph accumulator
        var codeLines: [String]? = nil
        var codeLang = ""

        func flushProse() {
            guard !prose.isEmpty else { return }
            let joined = prose.joined(separator: "\n")
                .trimmingCharacters(in: .whitespacesAndNewlines)
            if !joined.isEmpty { out.append(.paragraph(joined)) }
            prose = []
        }

        let lines = text.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        var lineIndex = 0
        while lineIndex < lines.count {
            let line = lines[lineIndex]
            lineIndex += 1

            // fenced code
            if line.hasPrefix("```") {
                if codeLines != nil {
                    out.append(.code(language: codeLang, codeLines!.joined(separator: "\n")))
                    codeLines = nil
                    codeLang = ""
                } else {
                    flushProse()
                    codeLang = String(line.dropFirst(3)).trimmingCharacters(in: .whitespaces)
                    codeLines = []
                }
                continue
            }
            if codeLines != nil { codeLines!.append(line); continue }

            let trimmed = line.trimmingCharacters(in: .whitespaces)

            if trimmed.isEmpty { flushProse(); continue }

            if let level = headingLevel(line, body: nil) {
                flushProse()
                out.append(.heading(level: level, text: trimmed.dropFirst(level).trimmingCharacters(in: .whitespaces)))
                continue
            }
            if isRule(trimmed) {
                flushProse()
                out.append(.rule)
                continue
            }
            if let (marker, body) = bulletParts(line) {
                flushProse()
                out.append(.bullet(text: body, depth: marker))
                continue
            }
            if let (n, body) = orderedParts(line) {
                flushProse()
                out.append(.numbered(n: n, text: body))
                continue
            }
            if trimmed.hasPrefix(">") {
                flushProse()
                out.append(.quote(String(trimmed.dropFirst()).trimmingCharacters(in: .whitespaces)))
                continue
            }
            if trimmed.hasPrefix("|"), trimmed.hasSuffix("|") || trimmed.contains("||") {
                // lineIndex already advanced: table starts at lineIndex - 1
                if let table = tryParseTable(lines: lines, from: lineIndex - 1) {
                    flushProse()
                    out.append(.table(headers: table.headers, rows: table.rows))
                    lineIndex = table.nextIndex
                    continue
                }
                // lone pipe line: fall through to paragraph
            }
            prose.append(line)
        }
        if let codeLines { // unterminated fence: show as code
            out.append(.code(language: codeLang, codeLines.joined(separator: "\n")))
        }
        flushProse()
        return out
    }

    private static func headingLevel(_ line: String, body: String?) -> Int? {
        var level = 0
        for ch in line {
            if ch == "#" { level += 1 } else { break }
        }
        guard (1...6).contains(level),
              line.count > level, line[line.index(line.startIndex, offsetBy: level)] == " "
        else { return nil }
        return level
    }

    private static func isRule(_ trimmed: String) -> Bool {
        guard trimmed.count >= 3 else { return false }
        return trimmed.allSatisfy { $0 == "-" } || trimmed.allSatisfy { $0 == "*" }
    }

    /// Returns (depth, body) for -/*/+ bullets, depth from leading indent.
    private static func bulletParts(_ line: String) -> (Int, String)? {
        var i = line.startIndex
        var spaces = 0
        while i < line.endIndex && line[i] == " " { spaces += 1; i = line.index(after: i) }
        let rest = line[i...]
        guard let first = rest.first, ["-", "*", "+"].contains(first) else { return nil }
        let afterMarker = rest.dropFirst()
        guard afterMarker.first == " " else { return nil }
        return (spaces / 2, String(afterMarker.dropFirst()))
    }

    private static func orderedParts(_ line: String) -> (Int, String)? {
        let digits = line.prefix(while: { $0.isNumber })
        guard !digits.isEmpty, let n = Int(digits) else { return nil }
        let rest = line.dropFirst(digits.count)
        guard rest.hasPrefix(". ") || rest.hasPrefix(") ") || rest.hasPrefix(".\t") else { return nil }
        return (n, String(rest.dropFirst(2)))
    }

    /// Pipe-table detection: header line, |---| separator, data rows.
    /// Requires >= 2 columns everywhere; rows with fewer cells are padded.
    /// Returns (headers, rows, nextIndex after the table).
    private static func tryParseTable(lines: [String], from start: Int) -> (headers: [String], rows: [[String]], nextIndex: Int)? {
        func cells(_ line: String) -> [String] {
            var s = line.trimmingCharacters(in: .whitespaces)
            if s.hasPrefix("|") { s = String(s.dropFirst()) }
            if s.hasSuffix("|") { s = String(s.dropLast()) }
            return s.components(separatedBy: "|").map { $0.trimmingCharacters(in: .whitespaces) }
        }
        func isSeparatorRow(_ line: String) -> Bool {
            let cs = cells(line)
            guard !cs.isEmpty else { return false }
            return cs.allSatisfy { cell in
                let c = cell.replacingOccurrences(of: ":", with: "")
                return c.count >= 1 && c.allSatisfy { $0 == "-" }
            }
        }

        guard start + 1 < lines.count else { return nil }
        let headerCells = cells(lines[start])
        guard headerCells.count >= 2 else { return nil }
        guard isSeparatorRow(lines[start + 1]) else { return nil }

        var rows: [[String]] = []
        var i = start + 2
        while i < lines.count {
            let t = lines[i].trimmingCharacters(in: .whitespaces)
            guard t.hasPrefix("|"), t.contains("|") else { break }
            var cs = cells(t)
            while cs.count < headerCells.count { cs.append("") }
            if cs.count > headerCells.count { cs = Array(cs.prefix(headerCells.count)) }
            rows.append(cs)
            i += 1
        }
        return (headerCells, rows, i)
    }

    /// Inline bold/italic/code within one text fragment.
    static func inline(_ s: String) -> AttributedString {
        if let a = try? AttributedString(
            markdown: s,
            options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace)
        ) { return a }
        return AttributedString(s)
    }
}

enum MdSegments {
    enum Segment: Equatable {
        case texty([MdBlock])
        case codey(language: String, String)
        case tabley(headers: [String], rows: [[String]])
    }

    static func segments(from blocks: [MdBlock]) -> [Segment] {
        var out: [Segment] = []
        var pending: [MdBlock] = []
        func flushPending() { if !pending.isEmpty { out.append(.texty(pending)); pending = [] } }
        for block in blocks {
            switch block {
            case .code(let lang, let code):
                flushPending()
                out.append(.codey(language: lang, code))
            case .table(let headers, let rows):
                flushPending()
                out.append(.tabley(headers: headers, rows: rows))
            default:
                pending.append(block)
            }
        }
        flushPending()
        return out
    }

    /// Flatten prose blocks to one attributed run set: heading/bullet/quote
    /// structure becomes fonts and glyphs, inline intent (bold/italic/code)
    /// is preserved per fragment.
    static func flatten(_ blocks: [MdBlock], scale: Double) -> AttributedString {
        var out = AttributedString()
        for block in blocks {
            var fragment: AttributedString
            var trailing = "\n\n" // paragraphs & headings get air; list lines don\'t
            switch block {
            case .heading(let level, let text):
                fragment = MarkdownParser.inline(text)
                fragment.font = ChatFont.heading(level, scale)
            case .bullet(let text, let depth):
                var marker = AttributedString(String(repeating: "  ", count: depth) + "\u{2022} ")
                marker.font = ChatFont.prose(scale)
                fragment = MarkdownParser.inline(text)
                if fragment.font == nil { fragment.font = ChatFont.prose(scale) }
                fragment = marker + fragment
                trailing = "\n"
            case .numbered(let n, let text):
                var marker = AttributedString("\(n). ")
                marker.font = ChatFont.prose(scale)
                fragment = MarkdownParser.inline(text)
                if fragment.font == nil { fragment.font = ChatFont.prose(scale) }
                fragment = marker + fragment
                trailing = "\n"
            case .quote(let text):
                fragment = MarkdownParser.inline(text)
                fragment.foregroundColor = .secondary
                if fragment.font == nil { fragment.font = ChatFont.prose(scale) }
                trailing = "\n"
            case .paragraph(let text):
                fragment = MarkdownParser.inline(text)
                if fragment.font == nil { fragment.font = ChatFont.prose(scale) }
            case .rule:
                var hr = AttributedString("\u{2015}\u{2015}\u{2015}")
                hr.font = ChatFont.prose(scale)
                hr.foregroundColor = .secondary
                fragment = hr
            case .code, .table:
                continue // handled via segments
            }
            out.append(fragment)
            out.append(AttributedString(trailing))
        }
        return out
    }

}

private struct TableView: View {
    let headers: [String]
    let rows: [[String]]
    @Environment(\.chatTextScale) private var scale

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            Grid(alignment: .topLeading, horizontalSpacing: 16, verticalSpacing: 6) {
                GridRow {
                    ForEach(headers.indices, id: \.self) { i in
                        Text(MarkdownParser.inline(headers[i]))
                            .font(ChatFont.detail(scale).weight(.semibold))
                    }
                }
                Divider().gridCellUnsizedAxes(.horizontal)
                ForEach(rows.indices, id: \.self) { r in
                    GridRow {
                        ForEach(rows[r].indices, id: \.self) { c in
                            Text(MarkdownParser.inline(rows[r][c]))
                                .font(ChatFont.detail(scale))
                                .textSelection(.enabled)
                        }
                    }
                    if r < rows.count - 1 { Divider().gridCellUnsizedAxes(.horizontal) }
                }
            }
            .padding(.vertical, 6)
            .padding(.horizontal, 10)
        }
        .background(Color.primary.opacity(0.025))
        .clipShape(RoundedRectangle(cornerRadius: 8))
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .stroke(Color.primary.opacity(0.10), lineWidth: 1)
        )
    }
}


/// Grid table: header row bold with divider, cells render inline md.
import SwiftUI

/// MdBlock-level markdown for completed transcript blocks: headings, bullets,
/// ordered lists, quotes, rules, and fenced code — with inline bold/italic/
/// `code` inside text. Zero dependencies; finished blocks render once.
struct MarkdownText: View {
    let blocks: [MdBlock]

    init(_ text: String) {
        self.blocks = MarkdownParser.blocks(text)
    }

    @Environment(\.chatTextScale) private var scale

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            ForEach(Array(segments.enumerated()), id: \.offset) { _, segment in
                switch segment {
                case .texty(let textBlocks):
                    // All prose flattened into ONE AttributedString/Text so
                    // text selection is contiguous across headings, bullets,
                    // and paragraphs (per-view selection can't cross views).
                    Text(MdSegments.flatten(textBlocks, scale: scale))
                        .lineSpacing(3.5 * scale)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .textSelection(.enabled)
                case .codey(let language, let code):
                    CodeBlockView(language: language, code: code)
                case .tabley(let headers, let rows):
                    TableView(headers: headers, rows: rows)
                }
            }
        }
    }

    private var segments: [MdSegments.Segment] { MdSegments.segments(from: blocks) }

    // MARK: segments for selection-contiguous rendering

    private struct CodeBlockView: View {
        let language: String
        let code: String
        @Environment(\.chatTextScale) private var scale

        // computed per render so scale changes take effect; one block's worth
        // of lines is cheap
        private var highlighted: AttributedString {
            let font = ChatFont.mono(scale)
            return code.split(separator: "\n", omittingEmptySubsequences: false)
                .map { DiffSyntax.highlight(String($0), path: "snippet.\(language)", font: font) }
                .reduce(into: AttributedString()) { acc, line in
                    if !acc.characters.isEmpty { acc.append(AttributedString("\n")) }
                    acc.append(line)
                }
        }

        var body: some View {
            VStack(alignment: .leading, spacing: 0) {
                if !language.isEmpty {
                    Text(language)
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, 8)
                        .padding(.top, 4)
                }
                ScrollView(.horizontal, showsIndicators: false) {
                    Text(highlighted)
                        .font(ChatFont.mono(scale))
                        .lineSpacing(1.5 * scale)
                        .textSelection(.enabled)
                        .padding(8)
                }
            }
            .background(Color(nsColor: .textBackgroundColor))
            .clipShape(RoundedRectangle(cornerRadius: 6))
            .overlay(
                RoundedRectangle(cornerRadius: 6)
                    .stroke(Color.primary.opacity(0.12), lineWidth: 1)
            )
        }
    }
}
