import SwiftUI

/// Block-level markdown for completed transcript blocks: headings, bullets,
/// ordered lists, quotes, rules, and fenced code — with inline bold/italic/
/// `code` inside text. Zero dependencies; finished blocks render once.
struct MarkdownText: View {
    let blocks: [Block]

    init(_ text: String) {
        self.blocks = Parser.blocks(text)
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
                    Text(Self.flatten(textBlocks, scale: scale))
                        .lineSpacing(3.5 * scale)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .textSelection(.enabled)
                case .codey(let language, let code):
                    CodeBlockView(language: language, code: code)
                }
            }
        }
    }

    private var segments: [Segment] { Self.segments(from: blocks) }

    // MARK: segments for selection-contiguous rendering

    enum Segment: Equatable {
        case texty([Block])
        case codey(language: String, String)
    }

    static func segments(from blocks: [Block]) -> [Segment] {
        var out: [Segment] = []
        var pending: [Block] = []
        for block in blocks {
            if case .code(let lang, let code) = block {
                if !pending.isEmpty { out.append(.texty(pending)); pending = [] }
                out.append(.codey(language: lang, code))
            } else {
                pending.append(block)
            }
        }
        if !pending.isEmpty { out.append(.texty(pending)) }
        return out
    }

    /// Flatten prose blocks to one attributed run set: heading/bullet/quote
    /// structure becomes fonts and glyphs, inline intent (bold/italic/code)
    /// is preserved per fragment.
    static func flatten(_ blocks: [Block], scale: Double) -> AttributedString {
        var out = AttributedString()
        for block in blocks {
            var fragment: AttributedString
            var trailing = "\n\n" // paragraphs & headings get air; list lines don\'t
            switch block {
            case .heading(let level, let text):
                fragment = Parser.inline(text)
                fragment.font = ChatFont.heading(level, scale)
            case .bullet(let text, let depth):
                var marker = AttributedString(String(repeating: "  ", count: depth) + "\u{2022} ")
                marker.font = ChatFont.prose(scale)
                fragment = Parser.inline(text)
                if fragment.font == nil { fragment.font = ChatFont.prose(scale) }
                fragment = marker + fragment
                trailing = "\n"
            case .numbered(let n, let text):
                var marker = AttributedString("\(n). ")
                marker.font = ChatFont.prose(scale)
                fragment = Parser.inline(text)
                if fragment.font == nil { fragment.font = ChatFont.prose(scale) }
                fragment = marker + fragment
                trailing = "\n"
            case .quote(let text):
                fragment = Parser.inline(text)
                fragment.foregroundColor = .secondary
                if fragment.font == nil { fragment.font = ChatFont.prose(scale) }
                trailing = "\n"
            case .paragraph(let text):
                fragment = Parser.inline(text)
                if fragment.font == nil { fragment.font = ChatFont.prose(scale) }
            case .rule:
                var hr = AttributedString("\u{2015}\u{2015}\u{2015}")
                hr.font = ChatFont.prose(scale)
                hr.foregroundColor = .secondary
                fragment = hr
            case .code:
                continue // handled via segments
            }
            out.append(fragment)
            out.append(AttributedString(trailing))
        }
        return out
    }

    // MARK: model

    enum Block: Equatable {
        case heading(level: Int, text: String)
        case bullet(text: String, depth: Int)
        case numbered(n: Int, text: String)
        case quote(String)
        case paragraph(String)
        case code(language: String, String)
        case rule
    }

    // MARK: parser

    enum Parser {
        static func blocks(_ text: String) -> [Block] {
            var out: [Block] = []
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

            for raw in text.split(separator: "\n", omittingEmptySubsequences: false) {
                let line = String(raw)

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

        /// Inline bold/italic/code within one text fragment.
        static func inline(_ s: String) -> AttributedString {
            if let a = try? AttributedString(
                markdown: s,
                options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace)
            ) { return a }
            return AttributedString(s)
        }
    }


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
