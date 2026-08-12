import SwiftUI

/// Block-level markdown for completed transcript blocks: headings, bullets,
/// ordered lists, quotes, rules, and fenced code — with inline bold/italic/
/// `code` inside text. Zero dependencies; finished blocks render once.
struct MarkdownText: View {
    let blocks: [Block]

    init(_ text: String) {
        self.blocks = Parser.blocks(text)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(blocks.enumerated()), id: \.offset) { _, block in
                BlockView(block: block)
            }
        }
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

    // MARK: block views

    private struct BlockView: View {
        let block: Block

        var body: some View {
            switch block {
            case .heading(let level, let text):
                Text(Parser.inline(text))
                    .font(Self.headingFont(level))
                    .fontWeight(.semibold)
                    .padding(.top, level <= 2 ? 6 : 3)
            case .bullet(let text, let depth):
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text("•")
                        .foregroundStyle(.secondary)
                    Text(Parser.inline(text))
                        .lineSpacing(2.5)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .padding(.leading, CGFloat(depth) * 14 + 2)
            case .numbered(let n, let text):
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text("\(n).")
                        .foregroundStyle(.secondary)
                        .monospacedDigit()
                    Text(Parser.inline(text))
                        .lineSpacing(2.5)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            case .quote(let text):
                HStack(alignment: .top, spacing: 8) {
                    RoundedRectangle(cornerRadius: 1)
                        .fill(Color.secondary.opacity(0.5))
                        .frame(width: 3)
                    Text(Parser.inline(text))
                        .lineSpacing(2.5)
                        .foregroundStyle(.secondary)
                }
            case .paragraph(let text):
                Text(Parser.inline(text))
                    .frame(maxWidth: .infinity, alignment: .leading)
            case .code(let language, let code):
                CodeBlockView(language: language, code: code)
            case .rule:
                Divider().opacity(0.6)
            }
        }

        static func headingFont(_ level: Int) -> Font {
            switch level {
            case 1: return .title2
            case 2: return .title3
            case 3: return .headline
            default: return .subheadline
            }
        }
    }

    private struct CodeBlockView: View {
        let language: String
        let code: String
        private let highlighted: AttributedString

        init(language: String, code: String) {
            self.language = language
            self.code = code
            // whole-block AttributedString so text selection stays contiguous
            let font = Font.system(.body, design: .monospaced)
            highlighted = code.split(separator: "\n", omittingEmptySubsequences: false)
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
                        .font(.system(.body, design: .monospaced))
                        .lineSpacing(1.5)
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
