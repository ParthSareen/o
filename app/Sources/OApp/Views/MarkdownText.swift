import SwiftUI

/// Fast markdown rendering: split fenced code blocks out (monospaced, plain
/// — the perf-critical path) and render prose segments with the system
/// markdown parser. Completed blocks render exactly once per view identity;
/// streaming text elsewhere renders as plain Text.
struct MarkdownText: View {
    enum Segment: Equatable {
        case prose(String)
        case code(language: String, String)
    }

    let segments: [Segment]

    init(_ text: String) {
        self.segments = Self.split(text)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            ForEach(Array(segments.enumerated()), id: \.offset) { _, segment in
                switch segment {
                case .prose(let text):
                    if let attributed = try? AttributedString(
                        markdown: text,
                        options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace)
                    ) {
                        Text(attributed)
                    } else {
                        Text(text)
                    }
                case .code(let language, let code):
                    VStack(alignment: .leading, spacing: 0) {
                        if !language.isEmpty {
                            Text(language)
                                .font(.caption2)
                                .foregroundStyle(.secondary)
                                .padding(.horizontal, 8)
                                .padding(.top, 4)
                        }
                        ScrollView(.horizontal, showsIndicators: false) {
                            Text(code.hasSuffix("\n") ? String(code.dropLast()) : code)
                                .font(.system(.body, design: .monospaced))
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
    }

    static func split(_ text: String) -> [Segment] {
        var segments: [Segment] = []
        var prose = ""
        var code = ""
        var language = ""
        var inCode = false
        for line in text.split(separator: "\n", omittingEmptySubsequences: false) {
            let str = String(line)
            if str.hasPrefix("```") {
                if inCode {
                    segments.append(.code(language: language, code))
                    code = ""
                    language = ""
                    inCode = false
                } else {
                    if !prose.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                        segments.append(.prose(prose))
                    }
                    prose = ""
                    language = String(str.dropFirst(3)).trimmingCharacters(in: .whitespaces)
                    inCode = true
                }
                continue
            }
            if inCode { code += str + "\n" } else { prose += str + "\n" }
        }
        if inCode { // unterminated fence: show what we have as code
            segments.append(.code(language: language, code))
        } else if !prose.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            segments.append(.prose(prose))
        }
        return segments
    }
}
