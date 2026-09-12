package transcriptview

// PartialPreviewNote is the one line a previewer prints above a transcript that
// stands for only PART of its session, because the database's capture of that
// session is not complete.
//
// Without it the pane is honest about nothing: a bounded projection of a long
// session and the whole of a short one draw identically, so a reader has no way
// to tell "this is the session" from "this is as much of the session as Peasant
// has recorded so far", and no way to know that harvesting again would show
// more. The line says both.
//
// It lives here, beside the renderer both surfaces draw with, so the share
// wizard's preview and the kickstart preview pane print the SAME sentence. Two
// copies of user-facing copy drift, and a reader who saw one wording on one
// screen and another on the next has to work out whether they mean the same
// thing.
//
// It is chrome, so it is lower-case like the rest of the panes that print it,
// and it is never applied to recorded content.
const PartialPreviewNote = "showing a partial preview of the session, ingest to preview entire session"
