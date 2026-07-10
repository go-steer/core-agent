# ErrImagePull

Sibling of `ImagePullBackOff` — kubelet transitions ErrImagePull →
ImagePullBackOff after a few failed attempts. Same fix matrix.

Chain to `references/ImagePullBackOff.md` and follow it. This file
exists so the router doesn't fall through to `_fallback.md` when
kubelet emits ErrImagePull instead of ImagePullBackOff (both are
common; both need the same investigation).
