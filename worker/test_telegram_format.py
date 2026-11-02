import html
from html.parser import HTMLParser
import re
import unittest

import telegram_format as tg


class ValidHTML(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.stack = []
        self.text = []

    def handle_starttag(self, tag, attrs):
        self.stack.append(tag)

    def handle_endtag(self, tag):
        if not self.stack or self.stack.pop() != tag:
            raise AssertionError('unbalanced tag ' + tag)

    def handle_data(self, data):
        self.text.append(data)


class TelegramFormatTests(unittest.TestCase):
    def test_answer_formatting(self):
        rendered = tg.markdown('# Result\n**Passed** & <not-html>\n`go test`\n```bash\necho "<ok>"\n```\n[docs](https://example.test/?a=1&b=2)')
        self.assertIn('<b>Result</b>', rendered)
        self.assertIn('<b>Passed</b> &amp; &lt;not-html&gt;', rendered)
        self.assertIn('<code>go test</code>', rendered)
        self.assertIn('<pre>echo "&lt;ok&gt;"\n</pre>', rendered)
        self.assertIn('<a href="https://example.test/?a=1&amp;b=2">docs</a>', rendered)
        self.assertNotIn('<not-html>', rendered)

    def test_unsafe_links_are_literal(self):
        self.assertNotIn('<a', tg.markdown('[x](javascript:alert)'))

    def test_incomplete_fence(self):
        self.assertEqual(tg.markdown('```\n<open>'), '<pre>&lt;open&gt;</pre>')

    def test_chunks_preserve_text_unicode_entities_and_nested_tags(self):
        body = '🧩 <a>& x\n' * 1400
        rendered = '<b>Summary</b>\n<pre><code>' + tg.escape(body) + '</code></pre>\n<i>Done</i>'
        parsed = ValidHTML(); parsed.feed(rendered)
        expected = ''.join(parsed.text)
        actual = []
        pieces = tg.chunks(rendered, limit=300)
        self.assertGreater(len(pieces), 1)
        for piece in pieces:
            self.assertLessEqual(len(piece.encode()), 300)
            parser = ValidHTML(); parser.feed(piece); parser.close()
            self.assertEqual(parser.stack, [])
            actual.extend(parser.text)
            self.assertNotRegex(re.sub(r'<[^>]*>', '', piece), r'&(?:amp|lt|gt)$')
        self.assertEqual(''.join(actual), expected)

    def test_preview_redacts_before_eliding(self):
        source = "API_KEY='private value' curl -H 'Authorization: Bearer private' https://u:pass@example.test piso_telegram_hidden 123456:abcdefghijklmnopqrstuvwxyzABCDEF"
        result = tg.preview(source, 500)
        for secret in ('private', 'pass@', 'piso_telegram_hidden', 'abcdefghijklmnopqrstuvwxyzABCDEF'):
            self.assertNotIn(secret, result)
        self.assertIn('[redacted]', result)
        self.assertTrue(tg.preview('x' * 200).endswith('…'))


if __name__ == '__main__':
    unittest.main()
