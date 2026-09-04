# C4 principles, with sources

The C4 model is small on purpose. Its rules are about discipline, not notation. This file lists
the principles this skill enforces, says where each one comes from, and carries the two
verbatim artefacts from c4model.com that the rules rest on: the notation rules and the diagram
review checklist. Quoted text was read from c4model.com on 2026-09-03.

## Where C4 comes from

Simon Brown built the model between about 2006 and 2011 while teaching software architecture to
developers. The four diagram names date from 2010. The "C4" label dates from 2011. Brown renamed the
fourth level from "classes" to "code" around 2015 to 2016. The FAQ names two ancestors: UML, and
Kruchten's 4+1 view model (IEEE Software, 1995). Brown describes C4 as a simplified reading of
both for teams that had stopped using UML during the agile years. The FAQ also names an aim: to
"reduce the gap between architecture models and actual code". That gap is George Fairbanks'
"model-code gap" from Just Enough Software Architecture, 2010.

The canonical, maintained form is https://c4model.com. Brown's long form is the Leanpub book
Software Architecture for Developers, Volume 2: Visualise, document and explore your software
architecture, and a shorter extract, The C4 model for visualising software architecture. His
conference talk "Visualising software architecture" is the original presentation, and "Diagrams
as code 2.0" (2021) carries the modelling-tool argument behind principle 7.

Source: https://c4model.com/faq

## The principles

| # | Principle | Source | Basis |
|---|---|---|---|
| 1 | Abstractions first, notation second. Agree on what person, system, container, and component mean. Draw them with any notation. | Home page: "notation independent" and "tooling independent". The abstractions pages define the terms. | Brown's rule, this skill's wording. |
| 2 | Zoom, do not mix. Each diagram shows one level. One element of the level above becomes the boundary of the level below. | Introduction: "maps of your code, at various levels of detail, in the same way you would use something like Google Maps to zoom in and out". Each diagram page fixes which element types it may show. | Skill wording. The zoom analogy is quoted in the Source column. The "do not mix" half is this skill's consolidation of the per-diagram scope rules. |
| 3 | Label everything. Every element has a name, a type, a technology where it applies, and a description. Every line has a label. | Notation page, quoted below. | Skill wording; the source rules are quoted below. |
| 4 | Title and key on every diagram. No unexplained acronyms. | Notation page and review checklist, quoted below. | Skill wording; the source rules are quoted below. |
| 5 | Match the level to the audience. Most teams need only levels 1 and 2. | Diagrams page: zoom levels "tell different stories to different audiences"; "the system context and container diagrams are sufficient for most software development teams". Each diagram page lists an intended audience. | Skill wording; the source sentences are quoted in the Source column. |
| 6 | Static structure by default. Runtime order goes on a dynamic diagram. Where things run goes on a deployment diagram. | Diagrams page: the four core diagrams are static structure diagrams; system landscape, dynamic, and deployment are supplementary. Container page: "Deployment information is better captured via one or more deployment diagrams, one per environment." | Skill wording; the source sentences are quoted in the Source column. |
| 7 | Consistency across diagrams. Draw every view from one model so that names, types, and technologies cannot disagree. | Tooling page: with diagramming tools "if you rename a box, you need to rename it across every diagram where it appears"; a modelling tool holds "a single definition of all elements and the relationships between them". | Brown's argument, this skill's label. |
| 8 | Diagrams reflect the code. Generate level 4 or skip it. Re-derive levels 1 to 3 from the source before redrawing. | FAQ on the model-code gap. Code page: "Ideally this diagram would be automatically generated using tooling". Diagrams FAQ: for code-level diagrams, "not create them at all or generate them on-demand". | Skill wording. The generate-or-skip part rests on the quotes in the Source column. The re-derive rule is this skill's practice; Brown says only that each level changes at a different rate. |

Two rules that this skill treats as hard also come straight from the container page:

- Not Docker: "I wanted a name that didn't imply anything about the physical nature of how that
  container is executed."
- Not libraries: "A container is a runtime construct, like an application; whereas Java JAR
  files, C# assemblies, DLLs, modules, etc are used to organise the code within those
  applications."

## The notation rules, verbatim

From https://c4model.com/diagrams/notation. C4 "is notation independent, and doesn't prescribe
any particular notation", but every notation must satisfy these.

Diagrams:

- "Every diagram should have a title describing the diagram type and scope (e.g. "System Context
  diagram for My Software System")."
- "Every diagram should have a key/legend explaining the notation being used (e.g. shapes,
  colours, border styles, line types, arrow heads, etc)."
- "Acronyms and abbreviations (business/domain or technology) should be understandable by all
  audiences, or explained in the diagram key/legend."

Elements:

- "The type of every element should be explicitly specified (e.g. Person, Software System,
  Container or Component)"
- "Every element should have a short description, to provide an 'at a glance' view of key
  responsibilities"
- "Every container and component should have a technology explicitly specified"

Relationships:

- "Every line should represent a unidirectional relationship"
- "Every line should be labelled, the label being consistent with the direction and intent of
  the relationship (e.g. dependency or data flow). Try to be as specific as possible with the
  label, ideally avoiding single words like, "Uses"."
- "Relationships between containers (typically these represent inter-process communication)
  should have a technology/protocol explicitly labelled."

C4 does not dictate colours and shapes. Use them consistently, explain them in the key, and do not
rely on colour alone.

## The diagram review checklist, verbatim

From https://c4model.com/diagrams/checklist. Walk it against the diagram as a reader who was
not in the room. Every "no" is a finding.

General:

- Does the diagram have a title?
- Do you understand what the diagram type is?
- Do you understand what the diagram scope is?
- Does the diagram have a key/legend?

Elements:

- Does every element have a name?
- Do you understand the type of every element? (i.e. the level of abstraction; e.g. software
  system, container, etc)
- Do you understand what every element does?
- Where applicable, do you understand the technology choices associated with every element?
- Do you understand the meaning of all acronyms and abbreviations used?
- Do you understand the meaning of all colours used?
- Do you understand the meaning of all shapes used?
- Do you understand the meaning of all icons used?
- Do you understand the meaning of all border styles used? (e.g. solid, dashed, etc)
- Do you understand the meaning of all element sizes used? (e.g. small vs large boxes)

Relationships:

- Does every arrow have a label describing the intent of that relationship?
- Does the description match the relationship direction?
- Where applicable, do you understand the technology choices associated with every
  relationship? (e.g. protocols for inter-process communication)
- Do you understand the meaning of all acronyms and abbreviations used?
- Do you understand the meaning of all colours used?
- Do you understand the meaning of all arrow heads used?
- Do you understand the meaning of all line styles used? (e.g. solid, dashed, etc)

## How the lint maps to the principles

`scripts/c4-lint.py` checks the parts of the checklist that a script can decide. The rest is the
reviewer's job.

| Lint code | Checklist item or principle |
|---|---|
| TITLE | "Does the diagram have a title?" and "Do you understand what the diagram type is / scope is?" |
| KEY | "Does the diagram have a key/legend?" |
| NO_ELEMENTS, TAG | "Do you understand the type of every element?" and "Every container and component should have a technology explicitly specified" |
| LEVEL_MIX | Principle 2, one level per diagram |
| ARROW_LABEL | "Every line should be labelled" |
| ARROW_TECH | Relationships between containers "should have a technology/protocol explicitly labelled" |
| STEP_NUMBER | Dynamic diagrams show order with numbered interactions |
| BIDIRECTIONAL | "Every line should represent a unidirectional relationship" |

Not checked by the lint, checked by the reviewer: whether the description says what the element
does, whether the label matches the arrow's direction, whether the acronyms are explained,
whether the elements and relationships match the code.
