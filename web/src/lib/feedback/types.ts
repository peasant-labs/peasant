/**
 * Closed sets and shared contracts for the in-app feedback widget.
 *
 * The widget is client-only: nothing here is a schema surface. Members that a
 * surface shows or that leave the app (a template name, a query-parameter id)
 * are `ClosedValue`s from `@peasant-labs/fairtrade/closed-value`, so a caller
 * compares the member itself and never writes the literal twice. `ALL_*` is the
 * single list tests and pickers walk; the `Missing*` checks fail to compile when
 * a member is added without it.
 *
 * `FeedbackStep`, `FeedbackEventType`, and `PendingCaptureKind` are plain literal
 * discriminants: they drive narrowing and are never shown or sent.
 *
 * This module imports nothing from peasant, so it can move to fairtrade when the
 * widget is lifted for village.
 */

import { closed } from '@peasant-labs/fairtrade/closed-value';

export const MAX_PICKED_ELEMENTS = 5;

// -- Closed sets -------------------------------------------------------------

/** What the user is filing. `template` is the issue-form file in `.github/ISSUE_TEMPLATE/`. */
export const FeedbackKind = Object.freeze({
  Problem: closed('problem', 'report a problem', { template: 'in-app-problem.yml' }),
  Feature: closed('feature', 'request a feature', { template: 'in-app-feature.yml' }),
} as const);
export type FeedbackKind = (typeof FeedbackKind)[keyof typeof FeedbackKind];
export const ALL_FEEDBACK_KINDS = Object.freeze([FeedbackKind.Problem, FeedbackKind.Feature] as const);
type MissingFeedbackKind = Exclude<FeedbackKind, (typeof ALL_FEEDBACK_KINDS)[number]>;
const _allFeedbackKinds: MissingFeedbackKind extends never ? true : never = true;

export const CaptureKind = Object.freeze({
  Element: closed('element', 'picked element'),
  Page: closed('page', 'whole page'),
} as const);
export type CaptureKind = (typeof CaptureKind)[keyof typeof CaptureKind];
export const ALL_CAPTURE_KINDS = Object.freeze([CaptureKind.Element, CaptureKind.Page] as const);
type MissingCaptureKind = Exclude<CaptureKind, (typeof ALL_CAPTURE_KINDS)[number]>;
const _allCaptureKinds: MissingCaptureKind extends never ? true : never = true;

/** Whether transcript content is painted over in a capture. Sensitive inputs are masked in both modes. */
export const MaskMode = Object.freeze({
  Masked: closed('masked', 'transcript masked'),
  Unmasked: closed('unmasked', 'transcript visible'),
} as const);
export type MaskMode = (typeof MaskMode)[keyof typeof MaskMode];
export const ALL_MASK_MODES = Object.freeze([MaskMode.Masked, MaskMode.Unmasked] as const);
type MissingMaskMode = Exclude<MaskMode, (typeof ALL_MASK_MODES)[number]>;
const _allMaskModes: MissingMaskMode extends never ? true : never = true;

export const MaskCategory = Object.freeze({
  TranscriptContent: closed('transcript-content', 'transcript content'),
  SensitiveInputs: closed('sensitive-inputs', 'sensitive inputs'),
} as const);
export type MaskCategory = (typeof MaskCategory)[keyof typeof MaskCategory];
export const ALL_MASK_CATEGORIES = Object.freeze([MaskCategory.TranscriptContent, MaskCategory.SensitiveInputs] as const);
type MissingMaskCategory = Exclude<MaskCategory, (typeof ALL_MASK_CATEGORIES)[number]>;
const _allMaskCategories: MissingMaskCategory extends never ? true : never = true;

/** What happened to the capture image when the user opened GitHub. */
export const ClipboardOutcome = Object.freeze({
  Copied: closed('copied', 'image copied; paste it into the issue'),
  Downloaded: closed('downloaded', 'image downloaded; drop it into the issue'),
  NotNeeded: closed('not-needed', 'no image attached'),
  Failed: closed('failed', 'the image could not be copied or downloaded'),
} as const);
export type ClipboardOutcome = (typeof ClipboardOutcome)[keyof typeof ClipboardOutcome];
export const ALL_CLIPBOARD_OUTCOMES = Object.freeze([
  ClipboardOutcome.Copied,
  ClipboardOutcome.Downloaded,
  ClipboardOutcome.NotNeeded,
  ClipboardOutcome.Failed,
] as const);
type MissingClipboardOutcome = Exclude<ClipboardOutcome, (typeof ALL_CLIPBOARD_OUTCOMES)[number]>;
const _allClipboardOutcomes: MissingClipboardOutcome extends never ? true : never = true;

/** Keyboard moves in the element picker. */
export const PickerDirection = Object.freeze({
  Parent: closed('parent', 'parent'),
  Child: closed('child', 'first child'),
  PrevSibling: closed('prev-sibling', 'previous sibling'),
  NextSibling: closed('next-sibling', 'next sibling'),
  NextLandmark: closed('next-landmark', 'next landmark'),
  PrevLandmark: closed('prev-landmark', 'previous landmark'),
} as const);
export type PickerDirection = (typeof PickerDirection)[keyof typeof PickerDirection];
export const ALL_PICKER_DIRECTIONS = Object.freeze([
  PickerDirection.Parent,
  PickerDirection.Child,
  PickerDirection.PrevSibling,
  PickerDirection.NextSibling,
  PickerDirection.NextLandmark,
  PickerDirection.PrevLandmark,
] as const);
type MissingPickerDirection = Exclude<PickerDirection, (typeof ALL_PICKER_DIRECTIONS)[number]>;
const _allPickerDirections: MissingPickerDirection extends never ? true : never = true;

/** Issue-form field ids. `wire` is both the YAML `id` and the prefill query-parameter name. */
export const IssueFormFieldId = Object.freeze({
  Description: closed('description', 'description'),
  Element: closed('element', 'picked element'),
  Screenshot: closed('screenshot', 'screenshot'),
  Environment: closed('environment', 'app context'),
} as const);
export type IssueFormFieldId = (typeof IssueFormFieldId)[keyof typeof IssueFormFieldId];
export const ALL_ISSUE_FORM_FIELD_IDS = Object.freeze([
  IssueFormFieldId.Description,
  IssueFormFieldId.Element,
  IssueFormFieldId.Screenshot,
  IssueFormFieldId.Environment,
] as const);
type MissingIssueFormFieldId = Exclude<IssueFormFieldId, (typeof ALL_ISSUE_FORM_FIELD_IDS)[number]>;
const _allIssueFormFieldIds: MissingIssueFormFieldId extends never ? true : never = true;

// -- Reducer discriminants (internal; never shown or sent) -------------------

export const FeedbackStep = {
  Closed: 'closed',
  MenuOpen: 'menu_open',
  AttachOptions: 'attach_options',
  Picking: 'picking',
  Capturing: 'capturing',
  Compose: 'compose',
  Submitted: 'submitted',
} as const;
export type FeedbackStep = (typeof FeedbackStep)[keyof typeof FeedbackStep];

export const PendingCaptureKind = {
  Element: 'element',
  All: 'all',
} as const;
export type PendingCaptureKind = (typeof PendingCaptureKind)[keyof typeof PendingCaptureKind];

export type PendingCapture =
  | { readonly kind: typeof PendingCaptureKind.Element; readonly elementId: number; readonly includePage: boolean }
  | { readonly kind: typeof PendingCaptureKind.All };

export type PickReturnStep = typeof FeedbackStep.AttachOptions | typeof FeedbackStep.Compose;

export const FeedbackEventType = {
  OpenMenu: 'open_menu',
  CloseAll: 'close_all',
  SelectKind: 'select_kind',
  ChangeKind: 'change_kind',
  ToggleAttachElement: 'toggle_attach_element',
  ToggleAttachPage: 'toggle_attach_page',
  ProceedFromAttach: 'proceed_from_attach',
  BackToAttach: 'back_to_attach',
  PickConfirmed: 'pick_confirmed',
  PickCancelled: 'pick_cancelled',
  CaptureSucceeded: 'capture_succeeded',
  CaptureFailed: 'capture_failed',
  RemoveCapture: 'remove_capture',
  SetMaskMode: 'set_mask_mode',
  UpdateDraft: 'update_draft',
  SubmitSucceeded: 'submit_succeeded',
  CopyCaptureSettled: 'copy_capture_settled',
  ReopenPicker: 'reopen_picker',
} as const;
export type FeedbackEventType = (typeof FeedbackEventType)[keyof typeof FeedbackEventType];

// -- Records -----------------------------------------------------------------

export interface ViewportSize {
  readonly width: number;
  readonly height: number;
}

export interface ElementBox {
  readonly top: number;
  readonly left: number;
  readonly width: number;
  readonly height: number;
}

/** The dev annotate overlay's target shape; kept so that overlay keeps working unchanged. */
export interface LegacyDomTarget {
  readonly selector: string;
  readonly label: string;
  readonly section: string;
  readonly snippet: string;
}

export interface PickedElementInfo {
  readonly id: number;
  readonly selector: string;
  readonly label: string;
  readonly section: string;
  /** Plain text, at most 120 chars; `[masked transcript content]` inside masked regions. */
  readonly textSnippet: string;
  readonly box: ElementBox;
}

export type PickedElementDraft = Omit<PickedElementInfo, 'id'>;

/** Auto-attached context. The user sees every field before anything leaves the app. */
export interface FeedbackContextInfo {
  /** `location.pathname` only; never the query string. */
  readonly route: string;
  readonly viewport: ViewportSize;
  /** The rendered theme, as `data-theme` reports it. Display only. */
  readonly theme: string;
  /** The peasant binary version, or `unknown` before the server reports one. */
  readonly appVersion: string;
  readonly userAgent: string;
}

export interface CaptureArtifact {
  readonly id: number;
  readonly kind: CaptureKind;
  /** Linked `PickedElementInfo.id` for element captures; `null` for page captures. */
  readonly elementId: number | null;
  readonly blob: Blob;
  readonly width: number;
  readonly height: number;
  readonly maskMode: MaskMode;
}

export interface IssueDraftInput {
  readonly kind: FeedbackKind;
  readonly title: string;
  readonly description: string;
  readonly elements: readonly PickedElementInfo[];
  readonly context: FeedbackContextInfo;
}

export interface FeedbackState {
  readonly step: FeedbackStep;
  readonly kind: FeedbackKind;
  readonly attachElement: boolean;
  readonly attachPage: boolean;
  readonly pickedElements: readonly PickedElementInfo[];
  readonly nextElementId: number;
  readonly nextCaptureId: number;
  readonly pendingCapture: PendingCapture | null;
  readonly pickReturnStep: PickReturnStep;
  readonly maskMode: MaskMode;
  readonly captures: readonly CaptureArtifact[];
  readonly captureError: string | null;
  readonly title: string;
  readonly description: string;
  readonly submittedUrl: string | null;
  readonly clipboardOutcome: ClipboardOutcome;
  readonly copiedCaptureId: number | null;
}

export type FeedbackEvent =
  | { readonly type: typeof FeedbackEventType.OpenMenu }
  | { readonly type: typeof FeedbackEventType.CloseAll }
  | { readonly type: typeof FeedbackEventType.SelectKind; readonly kind: FeedbackKind }
  | { readonly type: typeof FeedbackEventType.ChangeKind; readonly kind: FeedbackKind }
  | { readonly type: typeof FeedbackEventType.ToggleAttachElement }
  | { readonly type: typeof FeedbackEventType.ToggleAttachPage }
  | { readonly type: typeof FeedbackEventType.ProceedFromAttach }
  | { readonly type: typeof FeedbackEventType.BackToAttach }
  | { readonly type: typeof FeedbackEventType.PickConfirmed; readonly element: PickedElementDraft }
  | { readonly type: typeof FeedbackEventType.PickCancelled }
  | { readonly type: typeof FeedbackEventType.CaptureSucceeded; readonly captures: readonly CaptureArtifact[] }
  | {
      readonly type: typeof FeedbackEventType.CaptureFailed;
      readonly error: string;
      readonly partialCaptures?: readonly CaptureArtifact[];
    }
  | { readonly type: typeof FeedbackEventType.RemoveCapture; readonly id: number }
  | { readonly type: typeof FeedbackEventType.SetMaskMode; readonly maskMode: MaskMode }
  | { readonly type: typeof FeedbackEventType.UpdateDraft; readonly title?: string; readonly description?: string }
  | {
      readonly type: typeof FeedbackEventType.SubmitSucceeded;
      readonly url: string;
      readonly clipboardOutcome: ClipboardOutcome;
      readonly copiedCaptureId?: number | null;
    }
  | {
      readonly type: typeof FeedbackEventType.CopyCaptureSettled;
      readonly captureId: number;
      readonly clipboardOutcome: ClipboardOutcome;
    }
  | { readonly type: typeof FeedbackEventType.ReopenPicker };

// -- Ports -------------------------------------------------------------------

/** DOM knowledge the picker overlay needs, injected so the overlay stays host-neutral. */
export interface ElementPickerAdapter {
  isIgnoredElement(el: Element): boolean;
  isTooLargeRect(rect: Pick<DOMRect, 'width' | 'height'>, viewport: ViewportSize): boolean;
  labelFor(el: Element): string;
  sectionOf(el: Element): string;
  describe(el: Element): PickedElementDraft;
  step(current: Element | null, direction: PickerDirection, doc?: Document): Element | null;
}

export interface CaptureElementTarget {
  readonly id: number;
  readonly el: Element;
}

export interface CaptureRequest {
  readonly elements: readonly CaptureElementTarget[];
  readonly capturePage: boolean;
  readonly maskMode: MaskMode;
  readonly nextCaptureId?: number;
}

export interface CaptureBatchResult {
  readonly captures: readonly CaptureArtifact[];
  readonly errors: readonly string[];
}

// Keep the const bindings referenced so a reviewer can see the type tests are
// deliberate compile-time assertions rather than dead declarations.
void _allFeedbackKinds;
void _allCaptureKinds;
void _allMaskModes;
void _allMaskCategories;
void _allClipboardOutcomes;
void _allPickerDirections;
void _allIssueFormFieldIds;
