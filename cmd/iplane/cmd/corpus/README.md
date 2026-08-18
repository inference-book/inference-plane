# Load-driver corpus

`alice.txt` is the text of *Alice's Adventures in Wonderland* by Lewis
Carroll, from Project Gutenberg ebook 11, with the front matter and the
Project Gutenberg licence header and footer removed so that only the work
itself remains. Carroll died in 1898, so the work is in the public domain
worldwide, and the removed boilerplate is the only part of that file
Project Gutenberg asserts any terms over.

144,414 characters, roughly 36,000 tokens.

It is here because `iplane load --prompt-tokens` needs prompt text and
generated filler is the wrong thing to send. Repeated words tokenize
unrealistically, and more importantly they make every request's prefix
identical, so an engine with prefix caching reports a hit rate that says
more about the load generator than about the workload. Prose gives a
realistic token-per-character ratio and lets different requests draw
different windows.

A prompt longer than the corpus tiles it. That is a real limitation at
the very long contexts: a 1M-token prompt is the book read about
twenty-seven times, and an engine with prefix caching will notice. Say so
in any figure drawn from a run at that length.
